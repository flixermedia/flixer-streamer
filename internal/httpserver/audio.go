package httpserver

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/flixermedia/flixer-streamer/internal/media"
)

const maxLyricsBytes = 2 << 20

var audioTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg; codecs=opus",
	".wav":  "audio/wav",
	".aiff": "audio/aiff",
	".aif":  "audio/aiff",
}

func audioContentType(name string) string {
	return audioTypes[strings.ToLower(filepath.Ext(name))]
}

func isAudioFile(name string) bool { return audioContentType(name) != "" }

// handleLyrics serves the sidecar .lrc that sits next to the tokenised audio file.
func (h *Handler) handleLyrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	locator, ok := h.authorize(w, r, "/lyrics/")
	if !ok {
		return
	}
	if !isAudioFile(locator.RelativePath) {
		h.writeError(w, http.StatusNotFound, "lyrics_not_found", "No lyrics available")
		return
	}
	rel := strings.TrimSuffix(locator.RelativePath, filepath.Ext(locator.RelativePath)) + ".lrc"
	file, err := h.resolver.Open(locator.RootID, rel)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, media.ErrInvalidLocator) {
		h.writeError(w, http.StatusNotFound, "lyrics_not_found", "No lyrics available")
		return
	}
	if err != nil {
		h.logger.Error("open lyrics failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "lyrics_failed", "Unable to read lyrics")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLyricsBytes))
	if err != nil {
		h.logger.Error("read lyrics failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "lyrics_failed", "Unable to read lyrics")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(data)
}
