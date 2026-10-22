package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/flixermedia/flixer-streamer/internal/media"
)

const maxSubtitleBytes = 5 << 20

var subtitleLanguage = regexp.MustCompile(`^[A-Za-z]{2,3}(?:[-_][A-Za-z0-9]{2,4})?$`)

type subtitleTrack struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Label    string `json:"label,omitempty"`
}

func subtitleExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".srt" || ext == ".vtt" {
		return ext
	}
	return ""
}

// handleSubtitles lists the subtitle files beside a video, or with ?file= serves
// one as WebVTT. The token names the video; only files that share its base name
// can be read, and they are opened through the root-confined resolver.
func (h *Handler) handleSubtitles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	locator, ok := h.authorize(w, r, "/subtitles/")
	if !ok {
		return
	}
	video, err := h.resolver.Resolve(locator.RootID, locator.RelativePath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, media.ErrInvalidLocator) {
		h.writeError(w, http.StatusNotFound, "media_not_found", "Media is not available")
		return
	}
	if err != nil {
		h.logger.Error("resolve media failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "subtitles_failed", "Unable to read subtitles")
		return
	}
	prefix := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video)) + "."

	if file := r.URL.Query().Get("file"); file != "" {
		h.serveSubtitle(w, locator.RootID, locator.RelativePath, prefix, file)
		return
	}

	entries, err := os.ReadDir(filepath.Dir(video))
	if err != nil {
		h.logger.Error("list subtitles failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "subtitles_failed", "Unable to read subtitles")
		return
	}
	tracks := []subtitleTrack{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || subtitleExt(name) == "" || !strings.HasPrefix(name, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, prefix), filepath.Ext(name)), ".")
		track := subtitleTrack{ID: name, Language: "und"}
		if subtitleLanguage.MatchString(parts[0]) {
			track.Language = parts[0]
			parts = parts[1:]
		}
		track.Label = strings.Join(parts, " ")
		tracks = append(tracks, track)
	}
	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].Language != tracks[j].Language {
			return tracks[i].Language < tracks[j].Language
		}
		return tracks[i].ID < tracks[j].ID
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"tracks": tracks})
}

func (h *Handler) serveSubtitle(w http.ResponseWriter, rootID, videoRel, prefix, file string) {
	if file != filepath.Base(file) || strings.ContainsAny(file, `/\`) || subtitleExt(file) == "" || !strings.HasPrefix(file, prefix) {
		h.writeError(w, http.StatusNotFound, "subtitle_not_found", "Subtitle is not available")
		return
	}
	f, err := h.resolver.Open(rootID, path.Join(path.Dir(videoRel), file))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, media.ErrInvalidLocator) {
		h.writeError(w, http.StatusNotFound, "subtitle_not_found", "Subtitle is not available")
		return
	}
	if err != nil {
		h.logger.Error("open subtitle failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "subtitles_failed", "Unable to read subtitles")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSubtitleBytes+1))
	if err != nil || len(data) > maxSubtitleBytes {
		h.writeError(w, http.StatusInternalServerError, "subtitles_failed", "Unable to read subtitles")
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.WriteString(w, toWebVTT(data))
}

var srtTimestamp = regexp.MustCompile(`(\d{1,2}:\d{2}:\d{2}),(\d{1,3})`)

// toWebVTT converts SRT (or passes WebVTT through) as UTF-8 text.
func toWebVTT(data []byte) string {
	text := strings.TrimPrefix(string(data), "\ufeff")
	if !utf8.ValidString(text) {
		runes := make([]rune, len(data))
		for i, b := range data {
			runes[i] = rune(b)
		}
		text = string(runes)
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if strings.HasPrefix(text, "WEBVTT") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(line, "-->") {
			lines[i] = srtTimestamp.ReplaceAllString(line, "$1.$2")
		}
	}
	return "WEBVTT\n\n" + strings.Join(lines, "\n")
}
