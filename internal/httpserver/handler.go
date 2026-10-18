package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/token"
	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

type Options struct {
	Secret         []byte
	Resolver       *media.Resolver
	Transcoder     *transcode.Transcoder
	AllowedOrigins []string
	Logger         *slog.Logger
}

type Handler struct {
	secret         []byte
	resolver       *media.Resolver
	transcoder     *transcode.Transcoder
	allowedOrigins map[string]struct{}
	logger         *slog.Logger
}

func NewHandler(options Options) http.Handler {
	origins := make(map[string]struct{}, len(options.AllowedOrigins))
	for _, origin := range options.AllowedOrigins {
		origins[origin] = struct{}{}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		secret:         options.Secret,
		resolver:       options.Resolver,
		transcoder:     options.Transcoder,
		allowedOrigins: origins,
		logger:         logger,
	}
	return h.withLogging(http.HandlerFunc(h.route))
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	if !h.applyCORS(w, r) {
		h.writeError(w, http.StatusForbidden, "origin_not_allowed", "Origin is not allowed")
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, If-Modified-Since")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch r.URL.Path {
	case "/healthz":
		h.handleHealth(w, r)
	case "/stream/", "/stream":
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
	default:
		if strings.HasPrefix(r.URL.Path, "/stream/") {
			h.handleStream(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/info/") {
			h.handleInfo(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// authorize validates the token in the URL path after prefix and returns the
// canonical media path. It writes the error response itself on failure.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, prefix string) (token.Locator, bool) {
	rawToken := strings.TrimPrefix(r.URL.Path, prefix)
	if rawToken == "" || strings.Contains(rawToken, "/") {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
		return token.Locator{}, false
	}
	claims, err := token.Validate(rawToken, h.secret, time.Now())
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
		return token.Locator{}, false
	}
	locator, err := token.DecryptLocator(claims.MediaLocator, h.secret)
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, "invalid_token", "A valid stream token is required")
		return token.Locator{}, false
	}
	return locator, true
}

func parseOptions(r *http.Request) (transcode.Options, error) {
	var options transcode.Options
	query := r.URL.Query()
	if value := query.Get("height"); value != "" {
		height, err := strconv.Atoi(value)
		if err != nil || !transcode.ValidHeight(height) {
			return options, errors.New("height must be one of 240, 360, 480, 720, 1080")
		}
		options.Height = height
	}
	if value := query.Get("start"); value != "" {
		start, err := strconv.ParseFloat(value, 64)
		if err != nil || start < 0 || start > 7*24*3600 {
			return options, errors.New("start must be a non-negative number of seconds")
		}
		options.StartSeconds = start
	}
	return options, nil
}

// prepare resolves the media file and decides how it should be delivered.
func (h *Handler) prepare(w http.ResponseWriter, r *http.Request, locator token.Locator, options transcode.Options) (path string, probe transcode.Probe, mode transcode.Mode, ok bool) {
	path, err := h.resolver.Resolve(locator.RootID, locator.RelativePath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, media.ErrInvalidLocator) {
		h.writeError(w, http.StatusNotFound, "media_not_found", "Media is not available")
		return
	}
	if err != nil {
		h.logger.Error("resolve media failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "stream_failed", "Unable to serve media")
		return
	}
	probe, err = h.transcoder.Probe(r.Context(), path)
	if err != nil {
		h.logger.Error("probe media failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "stream_failed", "Unable to inspect media")
		return
	}
	return path, probe, transcode.Decide(filepath.Base(path), probe, options), true
}

func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	locator, ok := h.authorize(w, r, "/info/")
	if !ok {
		return
	}
	if h.transcoder == nil {
		h.writeError(w, http.StatusNotImplemented, "transcoding_unavailable", "ffmpeg is not available on this streamer")
		return
	}
	options, err := parseOptions(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_options", err.Error())
		return
	}
	_, probe, mode, ok := h.prepare(w, r, locator, options)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"playMethod":       mode,
		"source":           probe,
		"availableHeights": availableHeights(probe.Height),
	})
}

// availableHeights lists requested resolutions that are smaller than the source.
func availableHeights(sourceHeight int) []int {
	heights := []int{}
	for _, height := range transcode.AllowedHeights {
		if height < sourceHeight {
			heights = append(heights, height)
		}
	}
	return heights
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	locator, ok := h.authorize(w, r, "/stream/")
	if !ok {
		return
	}
	options, err := parseOptions(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_options", err.Error())
		return
	}

	if h.transcoder != nil {
		path, probe, mode, ok := h.prepare(w, r, locator, options)
		if !ok {
			return
		}
		if mode != transcode.ModeDirectPlay {
			h.streamProcessed(w, r, path, probe, mode, options)
			return
		}
	} else if options.Height != 0 || options.StartSeconds != 0 {
		h.writeError(w, http.StatusNotImplemented, "transcoding_unavailable", "ffmpeg is not available on this streamer")
		return
	}

	file, err := h.resolver.Open(locator.RootID, locator.RelativePath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, media.ErrInvalidLocator) {
		h.writeError(w, http.StatusNotFound, "media_not_found", "Media is not available")
		return
	}
	if err != nil {
		h.logger.Error("open media failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "stream_failed", "Unable to serve media")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		h.logger.Error("inspect media failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "stream_failed", "Unable to serve media")
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(info.Name())); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Length, Content-Range")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (h *Handler) streamProcessed(w http.ResponseWriter, r *http.Request, path string, probe transcode.Probe, mode transcode.Mode, options transcode.Options) {
	plan, err := transcode.BuildPlan(path, probe, mode, options)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_options", "Unsupported playback options")
		return
	}
	err = h.transcoder.Stream(r.Context(), plan, w, r.Method == http.MethodHead)
	switch {
	case err == nil || r.Context().Err() != nil:
	case errors.Is(err, transcode.ErrBusy):
		w.Header().Set("Retry-After", "5")
		h.writeError(w, http.StatusServiceUnavailable, "transcoder_busy", "Too many active transcodes, try again shortly")
	default:
		h.logger.Error("processed stream failed", "mode", mode, "error", err)
		// Headers are already sent once streaming has begun; writeError only
		// takes effect when ffmpeg failed before producing output.
		h.writeError(w, http.StatusInternalServerError, "stream_failed", "Unable to serve media")
	}
}

func (h *Handler) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	w.Header().Add("Vary", "Origin")
	if _, allowed := h.allowedOrigins[origin]; !allowed {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Length, Content-Range")
	return true
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": message,
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusRecorder) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(reader)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, reader)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (h *Handler) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		started := time.Now()
		next.ServeHTTP(recorder, r)
		route := "other"
		if r.URL.Path == "/healthz" {
			route = "/healthz"
		} else if strings.HasPrefix(r.URL.Path, "/stream/") {
			route = "/stream/{token}"
		} else if strings.HasPrefix(r.URL.Path, "/info/") {
			route = "/info/{token}"
		}
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		h.logger.Info("http request",
			"method", r.Method,
			"route", route,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}
