package httpserver

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/token"
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

var lyricsID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// openLyrics prefers the lyrics the indexer stored under the track id (they work for read-only
// libraries) and falls back to a sidecar .lrc next to the audio file.
func (h *Handler) openLyrics(locator token.Locator, itemID string) (*os.File, error) {
	if h.lyricsDir != "" && lyricsID.MatchString(itemID) {
		if f, err := os.Open(filepath.Join(h.lyricsDir, itemID+".lrc")); err == nil {
			return f, nil
		}
	}
	rel := strings.TrimSuffix(locator.RelativePath, filepath.Ext(locator.RelativePath)) + ".lrc"
	return h.resolver.Open(locator.RootID, rel)
}

// handleLyrics serves the track's lyrics from the lyrics store or the sidecar .lrc beside the audio file.
func (h *Handler) handleLyrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	locator, claims, ok := h.authorizeToken(w, strings.TrimPrefix(r.URL.Path, "/lyrics/"))
	if !ok {
		return
	}
	if !isAudioFile(locator.RelativePath) {
		h.writeError(w, http.StatusNotFound, "lyrics_not_found", "No lyrics available")
		return
	}
	file, err := h.openLyrics(locator, claims.MediaItemID)
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
