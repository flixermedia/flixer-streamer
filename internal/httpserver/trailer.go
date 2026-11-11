package httpserver

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

func validTrailerURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" &&
		strings.HasSuffix(strings.ToLower(u.Hostname()), ".googlevideo.com")
}

func (h *Handler) handleTrailer(w http.ResponseWriter, r *http.Request) {
	h.serveTrailer(w, r, fetchLive)
}

func (h *Handler) serveTrailer(w http.ResponseWriter, r *http.Request, fetch func(context.Context, string, http.Header) (*http.Response, error)) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/trailer/")
	claims, err := token.Validate(raw, h.secret, time.Now())
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid trailer token is required")
		return
	}
	locator, err := token.DecryptLocator(claims.MediaLocator, h.secret)
	if err != nil || locator.Type != "trailer" || !validTrailerURL(locator.URL) {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid trailer token is required")
		return
	}
	if !h.pluginAllows(r.Context(), claims) {
		h.writeError(w, http.StatusForbidden, "plugin_denied", "Playback is not allowed")
		return
	}
	if !h.live.acquire(r.Context()) {
		h.writeError(w, http.StatusServiceUnavailable, "upstream_busy", "Trailer proxy is busy")
		return
	}
	defer h.live.release()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	headers := make(http.Header)
	headers.Set("Range", r.Header.Get("Range"))
	headers.Set("If-Range", r.Header.Get("If-Range"))
	response, err := fetch(ctx, locator.URL, headers)
	if err != nil {
		h.logger.Warn("trailer upstream unavailable")
		h.writeError(w, http.StatusBadGateway, "upstream_unavailable", "Unable to load trailer")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		h.logger.Warn("trailer upstream returned an error", "status", response.StatusCode)
		h.writeError(w, http.StatusBadGateway, "upstream_unavailable", "Unable to load trailer")
		return
	}
	for _, name := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodGet {
		if _, err := io.Copy(w, response.Body); err != nil {
			h.logger.Warn("trailer stream interrupted", "error", err)
		}
	}
}
