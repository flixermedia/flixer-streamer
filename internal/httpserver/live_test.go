package httpserver

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

func TestLivePlaylistRewritesNestedSegmentsAndURIAttributes(t *testing.T) {
	now := time.Now()
	claims := token.Claims{
		Issuer: token.Issuer, Audience: token.Audience, Subject: "user-1", UserID: "user-1",
		MediaItemID: "item-1", MediaSourceID: "source-1", PlaySessionID: "play-1",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(), TokenID: "token-live",
	}
	proxy := newLiveProxy()
	body := []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nchild.m3u8?auth=child\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"keys/key.bin?auth=key\"\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\nseg/one.ts?auth=segment\n")
	rewritten, err := proxy.rewriteManifest(context.Background(), "signed.token.value", claims,
		"http://8.8.8.8:8080/live/master.m3u8?auth=master", body)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(rewritten), "\n")
	var resourcePaths []string
	for _, line := range lines {
		if strings.Contains(line, "/resource/") {
			resourcePaths = append(resourcePaths, line)
			if !strings.Contains(line, "/streamer/live/signed.token.value/resource/") {
				t.Errorf("rewritten URI lacks signed token: %q", line)
			}
		}
	}
	if len(resourcePaths) != 4 {
		t.Fatalf("rewritten resource count = %d, want 4: %s", len(resourcePaths), rewritten)
	}
	for _, resourcePath := range resourcePaths {
		resourceID := resourceIDFromPath(resourcePath)
		target, ok := proxy.resourceFor(claims, resourceID, now)
		if !ok {
			t.Errorf("resource %q was not registered", resourceID)
			continue
		}
		resourceURI := "/streamer/live/signed.token.value/resource/" + resourceID
		if _, err := url.ParseRequestURI(resourceURI); err != nil {
			t.Errorf("resource path %q is invalid: %v", resourceURI, err)
		}
		if _, err := parseLiveURL(target); err != nil {
			t.Errorf("registered URL %q is invalid: %v", target, err)
		}
	}
	refreshed, err := proxy.rewriteManifest(context.Background(), "signed.token.value", claims,
		"http://8.8.8.8:8080/live/master.m3u8?auth=master", body)
	if err != nil {
		t.Fatal(err)
	}
	if string(refreshed) != string(rewritten) || proxy.resourceTotal != len(resourcePaths) {
		t.Fatal("refresh did not reuse the existing opaque resource mappings")
	}
}

func resourceIDFromPath(line string) string {
	index := strings.LastIndex(line, "/resource/")
	if index < 0 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(line[index+len("/resource/"):]), "\"'")
}

func TestLiveSSRFValidationRejectsNonPublicAddressesAndPorts(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/stream.m3u8",
		"http://10.1.2.3/stream.m3u8",
		"http://169.254.169.254/latest/meta-data",
		"http://192.0.2.10/stream.m3u8",
		"http://[::1]/stream.m3u8",
		"http://[fec0::1]/stream.m3u8",
		"http://[3fff::1]/stream.m3u8",
		"http://user:pass@8.8.8.8/stream.m3u8",
		"http://8.8.8.8:8081/stream.m3u8",
		"file:///etc/passwd",
	} {
		t.Run(raw, func(t *testing.T) {
			target, err := parseLiveURL(raw)
			if err == nil {
				_, err = resolvePublicHost(context.Background(), target)
			}
			if err == nil {
				t.Fatalf("URL %q was accepted", raw)
			}
		})
	}
	credentialURL := (&url.URL{
		Scheme: "http", Host: "8.8.8.8", User: url.UserPassword("test", "password"), Path: "/stream.m3u8",
	}).String()
	if _, err := parseLiveURL(credentialURL); err == nil {
		t.Fatal("URL with credentials was accepted")
	}

	if !isPublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public IPv4 address was rejected")
	}
	if !isPublicIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("public IPv6 address was rejected")
	}
}

func TestLiveEndpointRejectsLocalUpstreamAndExpiredToken(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	now := time.Now()
	handler := NewHandler(Options{Secret: secret})
	localToken := createIPTVToken(t, secret, token.Locator{Type: "iptv", URL: "http://127.0.0.1:8080/master.m3u8"}, now.Add(time.Hour))
	localResponse := httptest.NewRecorder()
	handler.ServeHTTP(localResponse, httptest.NewRequest(http.MethodGet, "/streamer/live/"+localToken, nil))
	if localResponse.Code != http.StatusBadGateway {
		t.Fatalf("private upstream status = %d, want %d", localResponse.Code, http.StatusBadGateway)
	}

	expiredToken := createIPTVToken(t, secret, token.Locator{Type: "iptv", URL: "https://8.8.8.8/master.m3u8"}, now.Add(-time.Second))
	expiredResponse := httptest.NewRecorder()
	handler.ServeHTTP(expiredResponse, httptest.NewRequest(http.MethodGet, "/streamer/live/"+expiredToken, nil))
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want %d", expiredResponse.Code, http.StatusUnauthorized)
	}

	vodResponse := httptest.NewRecorder()
	handler.ServeHTTP(vodResponse, httptest.NewRequest(http.MethodGet, "/streamer/live/"+createStreamToken(t, secret, "movies", "movie.mp4"), nil))
	if vodResponse.Code != http.StatusUnauthorized {
		t.Fatalf("VOD token on live endpoint status = %d, want %d", vodResponse.Code, http.StatusUnauthorized)
	}

	unsupportedToken := createIPTVToken(t, secret, token.Locator{Type: "smb", URL: "https://8.8.8.8/master.m3u8"}, now.Add(time.Hour))
	unsupportedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unsupportedResponse, httptest.NewRequest(http.MethodGet, "/streamer/live/"+unsupportedToken, nil))
	if unsupportedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unsupported locator status = %d, want %d", unsupportedResponse.Code, http.StatusUnauthorized)
	}

	validToken := createIPTVToken(t, secret, token.Locator{Type: "iptv", URL: "https://8.8.8.8/master.m3u8"}, now.Add(time.Hour))
	resourceID, err := opaqueID()
	if err != nil {
		t.Fatal(err)
	}
	unmappedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unmappedResponse, httptest.NewRequest(http.MethodGet,
		"/streamer/live/"+validToken+"/resource/"+resourceID, nil))
	if unmappedResponse.Code != http.StatusNotFound {
		t.Fatalf("unmapped resource status = %d, want %d", unmappedResponse.Code, http.StatusNotFound)
	}

	expiredResourceResponse := httptest.NewRecorder()
	handler.ServeHTTP(expiredResourceResponse, httptest.NewRequest(http.MethodGet,
		"/streamer/live/"+expiredToken+"/resource/"+resourceID, nil))
	if expiredResourceResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired resource token status = %d, want %d", expiredResourceResponse.Code, http.StatusUnauthorized)
	}
}

func TestLiveResourceSessionExpires(t *testing.T) {
	proxy := newLiveProxy()
	claims := token.Claims{TokenID: "expired", ExpiresAt: time.Now().Add(-time.Second).Unix()}
	resourceID, ok := proxy.registerResource(claims, "https://8.8.8.8/seg.ts", time.Now().Add(-time.Minute))
	if !ok {
		t.Fatal("could not seed session")
	}
	if _, ok := proxy.resourceFor(claims, resourceID, time.Now()); ok {
		t.Fatal("expired session resource was served")
	}
	if len(proxy.sessions) != 0 {
		t.Fatalf("expired sessions remain cached: %d", len(proxy.sessions))
	}
}

func createIPTVToken(t *testing.T, secret []byte, locator token.Locator, expiresAt time.Time) string {
	t.Helper()
	plain, err := json.Marshal(locator)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256(append([]byte("flixer-streamer:media-locator:v1\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	encrypted := gcm.Seal(nonce, nonce, plain, []byte("flixer-streamer:media-locator:v1"))
	now := time.Now()
	claims, err := json.Marshal(token.Claims{
		Issuer: token.Issuer, Audience: token.Audience, Subject: "user-1", UserID: "user-1",
		MediaItemID: "item-1", MediaSourceID: "source-1", PlaySessionID: "play-1",
		MediaLocator: base64.RawURLEncoding.EncodeToString(encrypted),
		IssuedAt:     now.Unix(), ExpiresAt: expiresAt.Unix(), TokenID: "token-live",
	})
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
