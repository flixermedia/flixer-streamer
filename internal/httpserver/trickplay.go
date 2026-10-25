package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

var trickplaySheetName = regexp.MustCompile(`^[0-9]+\.jpg$`)

func trickplayKey(locator token.Locator) string {
	sum := sha256.Sum256([]byte(locator.RootID + "\n" + locator.RelativePath))
	return hex.EncodeToString(sum[:])[:32]
}

func (h *Handler) handleTrickplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	remaining := strings.TrimPrefix(r.URL.Path, "/trickplay/")
	rawToken, name, hasName := strings.Cut(remaining, "/")
	locator, _, ok := h.authorizeToken(w, rawToken)
	if !ok {
		return
	}
	if !hasName || name == "" || strings.Contains(name, "/") || (name != "index.json" && !trickplaySheetName.MatchString(name)) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	filePath := filepath.Join(h.trickplayDir, trickplayKey(locator), name)
	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	if err != nil {
		h.logger.Error("open trickplay file failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "trickplay_failed", "Unable to read trickplay")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	if name == "index.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=300")
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}
