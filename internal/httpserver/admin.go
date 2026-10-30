package httpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AdminKey derives the bearer key for the /admin endpoints from the shared
// stream secret, so Flixer needs no extra configuration. These endpoints are
// not exposed by the reverse proxy; only the Flixer API calls them.
func AdminKey(secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("flixer-streamer:admin:v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) authorizeAdmin(w http.ResponseWriter, r *http.Request) bool {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(AdminKey(h.secret))) != 1 {
		h.writeError(w, http.StatusUnauthorized, "invalid_admin_key", "Admin access denied")
		return false
	}
	return true
}

// handleAdmin serves internal streamer maintenance endpoints.
func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeAdmin(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/"), "/")
	id, hasID := strings.CutPrefix(path, "sessions/")

	switch {
	case path == "transcode-cache/cleanup" && r.Method == http.MethodPost:
		h.cleanupTranscodeCache(w)
	case path == "sessions" && r.Method == http.MethodGet:
		sessions, maxConcurrent := []any{}, 0
		if h.transcoder != nil {
			maxConcurrent = h.transcoder.MaxConcurrent()
			for _, session := range h.transcoder.Sessions() {
				sessions = append(sessions, session)
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"transcodingAvailable": h.transcoder != nil,
			"maxTranscodes":        maxConcurrent,
			"activeTranscodes":     h.activeTranscodes(),
			"sessions":             sessions,
		})
	case hasID && id != "" && !strings.Contains(id, "/") && r.Method == http.MethodDelete:
		if h.transcoder == nil || !h.transcoder.CancelSession(id) {
			h.writeError(w, http.StatusNotFound, "session_not_found", "Session not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "sessions" || hasID || path == "transcode-cache/cleanup":
		allow := "GET, DELETE"
		if path == "transcode-cache/cleanup" {
			allow = http.MethodPost
		}
		w.Header().Set("Allow", allow)
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) cleanupTranscodeCache(w http.ResponseWriter) {
	removed, err := removeExpiredTranscodeFiles(h.transcodeDir, time.Now().Add(-24*time.Hour))
	if err != nil {
		h.logger.Error("could not clean transcode cache", "error", err)
		h.writeError(w, http.StatusInternalServerError, "transcode_cache_cleanup_failed", "Could not clean transcode cache")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]int{"removed": removed})
}

func removeExpiredTranscodeFiles(directory string, cutoff time.Time) (int, error) {
	if directory == "" {
		return 0, errors.New("transcode directory is not configured")
	}
	root, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !root.IsDir() || root.Mode()&fs.ModeSymlink != 0 {
		return 0, errors.New("transcode directory must be a real directory")
	}

	removed := 0
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == directory || entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}
