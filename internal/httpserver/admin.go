package httpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
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

// handleAdmin serves GET /streamer/admin/sessions and DELETE /streamer/admin/sessions/{id}.
func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeAdmin(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/"), "/")
	id, hasID := strings.CutPrefix(path, "sessions/")

	switch {
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
	case path == "sessions" || hasID:
		w.Header().Set("Allow", "GET, DELETE")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
	default:
		http.NotFound(w, r)
	}
}
