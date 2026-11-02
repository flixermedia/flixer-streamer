package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

const (
	liveMaxSessions       = 512
	liveMaxConcurrent     = 32
	liveMaxResources      = 16_384
	liveMaxResourcesPerID = 10_000
	liveMaxPlaylistBytes  = 2 << 20
	liveMaxResourceBytes  = 128 << 20
	liveRequestTimeout    = 90 * time.Second
	livePlaylistTimeout   = 20 * time.Second
)

type liveResource struct {
	url          string
	base         bool
	placeholders []dashURLPlaceholder
}

type liveSession struct {
	expiresAt time.Time
	lastUsed  time.Time
	resources map[string]liveResource
	idsByURL  map[string]string
}

type liveProxy struct {
	mu            sync.Mutex
	sessions      map[string]*liveSession
	resourceTotal int
	requests      chan struct{}
}

func newLiveProxy() *liveProxy {
	return &liveProxy{
		sessions: make(map[string]*liveSession),
		requests: make(chan struct{}, liveMaxConcurrent),
	}
}

func (h *Handler) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	route := strings.TrimPrefix(r.URL.Path, "/live/")
	parts := strings.Split(route, "/")
	if len(parts) != 1 && (len(parts) < 3 || parts[1] != "resource") {
		http.NotFound(w, r)
		return
	}
	rawToken := parts[0]
	if rawToken == "" || strings.Contains(rawToken, "/") {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
		return
	}
	claims, err := token.Validate(rawToken, h.secret, time.Now())
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
		return
	}
	locator, err := token.DecryptLocator(claims.MediaLocator, h.secret)
	if err != nil || !locator.IsIPTV() {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
		return
	}
	if !h.live.acquire(r.Context()) {
		h.writeError(w, http.StatusServiceUnavailable, "upstream_busy", "Live HLS proxy is busy")
		return
	}
	defer h.live.release()

	if len(parts) == 1 {
		h.serveLivePlaylist(w, r, rawToken, claims, locator.URL)
		return
	}
	resourceID := parts[2]
	if !validOpaqueID(resourceID) {
		http.NotFound(w, r)
		return
	}
	resource, ok := h.live.resourceEntryFor(claims, resourceID, time.Now())
	if !ok {
		h.writeError(w, http.StatusNotFound, "resource_not_found", "HLS resource is not available")
		return
	}
	suffix := strings.Join(parts[3:], "/")
	upstreamURL, err := resolveLiveResource(resource, suffix, r.URL.Query())
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_resource", "HLS resource is not available")
		return
	}
	h.serveLiveResource(w, r, rawToken, claims, upstreamURL)
}

func (p *liveProxy) acquire(ctx context.Context) bool {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case p.requests <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (p *liveProxy) release() {
	<-p.requests
}

func (h *Handler) serveLivePlaylist(w http.ResponseWriter, r *http.Request, rawToken string, claims token.Claims, upstreamURL string) {
	ctx, cancel := context.WithTimeout(r.Context(), livePlaylistTimeout)
	defer cancel()
	response, err := fetchLive(ctx, upstreamURL, nil)
	if err != nil {
		h.logger.Warn("live manifest fetch rejected")
		h.writeError(w, http.StatusBadGateway, "upstream_unavailable", "Unable to load live manifest")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		h.writeError(w, http.StatusBadGateway, "upstream_unavailable", "Unable to load live manifest")
		return
	}
	body, err := readLiveBody(response.Body, response.ContentLength, liveMaxPlaylistBytes)
	if err != nil {
		h.logger.Warn("invalid live manifest response")
		h.writeError(w, http.StatusBadGateway, "invalid_manifest", "Upstream did not return a valid HLS or DASH manifest")
		return
	}
	if isPlaylist(body) {
		rewritten, rewriteErr := h.live.rewriteManifest(ctx, rawToken, claims, liveResponseURL(response, upstreamURL), body)
		if rewriteErr != nil {
			h.logger.Warn("live playlist rewrite failed")
			h.writeError(w, http.StatusBadGateway, "invalid_playlist", "Unable to process live playlist")
			return
		}
		h.writePlaylist(w, rewritten)
		return
	}
	if isDASHManifest(body, response.Header.Get("Content-Type"), upstreamURL) {
		rewritten, rewriteErr := h.live.rewriteDASHManifest(ctx, rawToken, claims, liveResponseURL(response, upstreamURL), body)
		if rewriteErr != nil {
			h.logger.Warn("live DASH manifest rewrite failed")
			h.writeError(w, http.StatusBadGateway, "invalid_manifest", "Unable to process live DASH manifest")
			return
		}
		h.writeDASHManifest(w, rewritten)
		return
	}
	h.logger.Warn("invalid live manifest response")
	h.writeError(w, http.StatusBadGateway, "invalid_manifest", "Upstream did not return a valid HLS or DASH manifest")
}

func liveResponseURL(response *http.Response, fallback string) string {
	if response.Request != nil && response.Request.URL != nil {
		return response.Request.URL.String()
	}
	return fallback
}

func (h *Handler) serveLiveResource(w http.ResponseWriter, r *http.Request, rawToken string, claims token.Claims, upstreamURL string) {
	ctx, cancel := context.WithTimeout(r.Context(), liveRequestTimeout)
	defer cancel()
	headers := make(http.Header)
	if value := r.Header.Get("Range"); value != "" {
		headers.Set("Range", value)
	}
	if value := r.Header.Get("If-Range"); value != "" {
		headers.Set("If-Range", value)
	}
	response, err := fetchLive(ctx, upstreamURL, headers)
	if err != nil {
		h.logger.Warn("live resource fetch rejected")
		h.writeError(w, http.StatusBadGateway, "upstream_unavailable", "Unable to load live media resource")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		h.writeError(w, http.StatusBadGateway, "upstream_unavailable", "Unable to load live media resource")
		return
	}
	if response.ContentLength > liveMaxResourceBytes {
		h.writeError(w, http.StatusBadGateway, "resource_too_large", "Live media resource exceeds the size limit")
		return
	}

	playlistHint := isPlaylistURL(upstreamURL) || isPlaylistType(response.Header.Get("Content-Type"))
	dashHint := isDASHURL(upstreamURL) || isDASHManifestType(response.Header.Get("Content-Type"))
	resolvedUpstreamURL := liveResponseURL(response, upstreamURL)
	prefix := make([]byte, 512)
	n, _ := io.ReadFull(response.Body, prefix)
	prefix = prefix[:n]
	if playlistHint || isPlaylist(prefix) {
		body, readErr := readLiveBody(io.MultiReader(bytes.NewReader(prefix), response.Body),
			response.ContentLength, liveMaxPlaylistBytes)
		if readErr != nil || !isPlaylist(body) {
			h.writeError(w, http.StatusBadGateway, "invalid_playlist", "Unable to process HLS playlist")
			return
		}
		rewritten, rewriteErr := h.live.rewriteManifest(ctx, rawToken, claims, resolvedUpstreamURL, body)
		if rewriteErr != nil {
			h.writeError(w, http.StatusBadGateway, "invalid_playlist", "Unable to process live playlist")
			return
		}
		h.writePlaylist(w, rewritten)
		return
	}
	if dashHint || isDASHXMLPrefix(prefix) {
		body, readErr := readLiveBody(io.MultiReader(bytes.NewReader(prefix), response.Body),
			response.ContentLength, liveMaxPlaylistBytes)
		if readErr != nil || !isDASHManifest(body, response.Header.Get("Content-Type"), resolvedUpstreamURL) {
			h.writeError(w, http.StatusBadGateway, "invalid_manifest", "Unable to process DASH manifest")
			return
		}
		rewritten, rewriteErr := h.live.rewriteDASHManifest(ctx, rawToken, claims, resolvedUpstreamURL, body)
		if rewriteErr != nil {
			h.writeError(w, http.StatusBadGateway, "invalid_manifest", "Unable to process live DASH manifest")
			return
		}
		h.writeDASHManifest(w, rewritten)
		return
	}
	response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), response.Body))

	if response.StatusCode == http.StatusPartialContent {
		w.Header().Set("Content-Range", response.Header.Get("Content-Range"))
	}
	if response.Header.Get("Accept-Ranges") != "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	contentType := safeContentType(response.Header.Get("Content-Type"))
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(&liveLimitWriter{writer: w, limit: liveMaxResourceBytes}, io.LimitReader(response.Body, liveMaxResourceBytes+1))
	}
}

type liveLimitWriter struct {
	writer io.Writer
	limit  int64
	wrote  int64
}

func (w *liveLimitWriter) Write(data []byte) (int, error) {
	if w.wrote+int64(len(data)) > w.limit {
		remaining := w.limit - w.wrote
		if remaining > 0 {
			n, err := w.writer.Write(data[:remaining])
			return n, errors.Join(err, errors.New("HLS resource exceeds the size limit"))
		}
		return 0, errors.New("HLS resource exceeds the size limit")
	}
	n, err := w.writer.Write(data)
	w.wrote += int64(n)
	return n, err
}

func (h *Handler) writePlaylist(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *Handler) writeDASHManifest(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (p *liveProxy) rewriteManifest(ctx context.Context, rawToken string, claims token.Claims, baseURL string, body []byte) ([]byte, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.New("invalid playlist URL")
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	validatedHosts := make(map[string]struct{})
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			rewritten, err := p.rewriteURIAttributes(ctx, rawToken, claims, base, line, validatedHosts)
			if err != nil {
				return nil, err
			}
			lines[i] = rewritten
			continue
		}
		resource, err := p.registerReference(ctx, rawToken, claims, base, trimmed, validatedHosts)
		if err != nil {
			return nil, err
		}
		lines[i] = resource
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func (p *liveProxy) rewriteURIAttributes(ctx context.Context, rawToken string, claims token.Claims, base *url.URL, line string, validatedHosts map[string]struct{}) (string, error) {
	type replacement struct {
		start int
		end   int
		value string
	}
	var changes []replacement
	for start := 0; start < len(line); {
		end := start
		quoted := false
		for end < len(line) {
			switch line[end] {
			case '"':
				quoted = !quoted
			case ',':
				if !quoted {
					goto attributeEnd
				}
			}
			end++
		}
	attributeEnd:
		attribute := line[start:end]
		eq := strings.IndexByte(attribute, '=')
		key := ""
		if eq > 0 {
			key = strings.TrimSpace(attribute[:eq])
			if colon := strings.LastIndexByte(key, ':'); colon >= 0 {
				key = key[colon+1:]
			}
		}
		if key == "URI" {
			valueStart := start + eq + 1
			for valueStart < end && (line[valueStart] == ' ' || line[valueStart] == '\t') {
				valueStart++
			}
			valueEnd := end
			quote := byte(0)
			if valueStart < end && (line[valueStart] == '"' || line[valueStart] == '\'') {
				quote = line[valueStart]
				valueStart++
				if valueEnd > valueStart && line[valueEnd-1] == quote {
					valueEnd--
				}
			}
			uri := line[valueStart:valueEnd]
			resource, err := p.registerReference(ctx, rawToken, claims, base, uri, validatedHosts)
			if err != nil {
				return "", err
			}
			changes = append(changes, replacement{start: valueStart, end: valueEnd, value: resource})
		}
		if end == len(line) {
			break
		}
		start = end + 1
	}
	for i := len(changes) - 1; i >= 0; i-- {
		change := changes[i]
		line = line[:change.start] + change.value + line[change.end:]
	}
	return line, nil
}

func (p *liveProxy) registerReference(ctx context.Context, rawToken string, claims token.Claims, base *url.URL, reference string, validatedHosts map[string]struct{}) (string, error) {
	ref, err := url.Parse(strings.TrimSpace(reference))
	if err != nil || (ref.Scheme == "" && ref.Host == "" && ref.Path == "" && ref.RawQuery == "") {
		return "", errors.New("invalid HLS URI")
	}
	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	safeURL, err := parseLiveURL(resolved.String())
	if err != nil {
		return "", errors.New("HLS URI is not a permitted public URL")
	}
	host := strings.ToLower(safeURL.Hostname())
	if _, ok := validatedHosts[host]; !ok {
		ips, err := resolvePublicHost(ctx, safeURL)
		if err != nil || len(ips) == 0 {
			return "", errors.New("HLS URI is not a permitted public URL")
		}
		validatedHosts[host] = struct{}{}
	}
	id, ok := p.registerResource(claims, resolved.String(), time.Now())
	if !ok {
		return "", errors.New("too many live playlist resources")
	}
	return "/streamer/live/" + url.PathEscape(rawToken) + "/resource/" + id, nil
}

func (p *liveProxy) registerResource(claims token.Claims, target string, now time.Time) (string, bool) {
	return p.registerResourceEntry(claims, target, liveResource{url: target}, now)
}

func (p *liveProxy) registerResourceEntry(claims token.Claims, key string, resource liveResource, now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanExpired(now)
	session := p.sessions[claims.TokenID]
	if session == nil {
		if len(p.sessions) >= liveMaxSessions {
			var oldestID string
			var oldest time.Time
			for key, candidate := range p.sessions {
				if oldestID == "" || candidate.lastUsed.Before(oldest) {
					oldestID, oldest = key, candidate.lastUsed
				}
			}
			p.removeSession(oldestID)
		}
		session = &liveSession{expiresAt: time.Unix(claims.ExpiresAt, 0), resources: make(map[string]liveResource)}
		session.idsByURL = make(map[string]string)
		p.sessions[claims.TokenID] = session
	}
	if id := session.idsByURL[key]; id != "" {
		session.lastUsed = now
		return id, true
	}
	if len(session.resources) >= liveMaxResourcesPerID || p.resourceTotal >= liveMaxResources {
		return "", false
	}
	id, err := opaqueID()
	if err != nil {
		return "", false
	}
	session.resources[id] = resource
	session.idsByURL[key] = id
	p.resourceTotal++
	session.lastUsed = now
	return id, true
}

func (p *liveProxy) resourceFor(claims token.Claims, id string, now time.Time) (string, bool) {
	resource, ok := p.resourceEntryFor(claims, id, now)
	return resource.url, ok
}

func (p *liveProxy) resourceEntryFor(claims token.Claims, id string, now time.Time) (liveResource, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanExpired(now)
	session := p.sessions[claims.TokenID]
	if session == nil {
		return liveResource{}, false
	}
	resource, ok := session.resources[id]
	if ok {
		session.lastUsed = now
	}
	return resource, ok
}

func (p *liveProxy) cleanExpired(now time.Time) {
	for id, session := range p.sessions {
		if !now.Before(session.expiresAt) {
			p.removeSession(id)
		}
	}
}

func (p *liveProxy) removeSession(id string) {
	if session := p.sessions[id]; session != nil {
		p.resourceTotal -= len(session.resources)
		delete(p.sessions, id)
	}
}

func opaqueID() (string, error) {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func validOpaqueID(value string) bool {
	if len(value) != 24 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == 18
}

func readLiveBody(body io.Reader, contentLength, limit int64) ([]byte, error) {
	if contentLength > limit {
		return nil, errors.New("upstream response exceeds size limit")
	}
	result, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(result)) > limit {
		return nil, errors.New("upstream response exceeds size limit")
	}
	return result, nil
}

func isPlaylist(body []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(body)), "#EXTM3U")
}

func isPlaylistType(contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return strings.Contains(strings.ToLower(mediaType), "mpegurl")
}

func isPlaylistURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8")
}

func safeContentType(value string) string {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || strings.ContainsAny(mediaType, "\r\n") {
		return ""
	}
	return mime.FormatMediaType(mediaType, params)
}

func fetchLive(ctx context.Context, rawURL string, headers http.Header) (*http.Response, error) {
	current, err := parseLiveURL(rawURL)
	if err != nil {
		return nil, err
	}
	for redirects := 0; ; redirects++ {
		addresses, err := resolvePublicHost(ctx, current)
		if err != nil {
			return nil, err
		}
		port := current.Port()
		if port == "" {
			if current.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		allowedHost := current.Hostname()
		transport := &http.Transport{
			DisableKeepAlives:      true,
			DisableCompression:     true,
			MaxResponseHeaderBytes: 32 << 10,
			TLSHandshakeTimeout:    8 * time.Second,
			ResponseHeaderTimeout:  15 * time.Second,
			DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
				host, dialPort, splitErr := net.SplitHostPort(address)
				if splitErr != nil || !strings.EqualFold(host, allowedHost) || dialPort != port {
					return nil, errors.New("unexpected upstream dial target")
				}
				dialer := net.Dialer{Timeout: 8 * time.Second}
				var lastErr error
				for _, ip := range addresses {
					conn, dialErr := dialer.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				return nil, lastErr
			},
		}
		client := &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			transport.CloseIdleConnections()
			return nil, err
		}
		request.Header.Set("Accept", "application/vnd.apple.mpegurl, application/octet-stream, */*")
		request.Header.Set("Accept-Encoding", "identity")
		if headers != nil {
			request.Header.Set("Range", headers.Get("Range"))
			request.Header.Set("If-Range", headers.Get("If-Range"))
		}
		response, err := client.Do(request)
		transport.CloseIdleConnections()
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 300 || response.StatusCode > 399 {
			return response, nil
		}
		location := response.Header.Get("Location")
		_ = response.Body.Close()
		if location == "" || redirects >= 4 {
			return nil, errors.New("invalid or excessive upstream redirect")
		}
		ref, err := url.Parse(location)
		if err != nil {
			return nil, err
		}
		current = current.ResolveReference(ref)
		current.Fragment = ""
		if _, err := parseLiveURL(current.String()); err != nil {
			return nil, err
		}
	}
}

func parseLiveURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Opaque != "" {
		return nil, errors.New("invalid upstream URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("upstream URL scheme is not permitted")
	}
	if parsed.User != nil || parsed.Hostname() == "" || strings.Contains(parsed.Hostname(), "%") {
		return nil, errors.New("upstream URL host is not permitted")
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || (value != 80 && value != 443 && value != 8080 && value != 8443) {
			return nil, errors.New("upstream URL port is not permitted")
		}
	}
	parsed.Fragment = ""
	return parsed, nil
}

var blockedIPRanges = func() []*net.IPNet {
	values := []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fec0::/10",
	}
	ranges := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, _ := net.ParseCIDR(value)
		ranges = append(ranges, network)
	}
	return ranges
}()

func resolvePublicHost(ctx context.Context, target *url.URL) ([]net.IP, error) {
	safe, err := parseLiveURL(target.String())
	if err != nil {
		return nil, err
	}
	host := safe.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, errors.New("upstream IP address is not public")
		}
		return []net.IP{ip}, nil
	}
	records, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream host: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("upstream host has no addresses")
	}
	addresses := make([]net.IP, 0, len(records))
	for _, record := range records {
		if !isPublicIP(record.IP) {
			return nil, errors.New("upstream host resolves to a non-public IP address")
		}
		addresses = append(addresses, record.IP)
	}
	return addresses, nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, blocked := range blockedIPRanges {
		if blocked.Contains(ip) {
			return false
		}
	}
	return true
}
