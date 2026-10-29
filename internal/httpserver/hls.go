package httpserver

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

// handleHLS serves /streamer/hls/<token>/index.m3u8 and /streamer/hls/<token>/<n>.ts. iOS Safari
// cannot play the endless fragmented MP4 used elsewhere, so it gets VOD HLS
// whose segments are encoded on demand.
func (h *Handler) handleHLS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	rawToken, file, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/hls/"), "/")
	if !found || file == "" {
		h.writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	locator, claims, ok := h.authorizeToken(w, rawToken)
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
	path, probe, _, ok := h.prepare(w, r, locator, transcode.Options{})
	if !ok {
		return
	}

	if file == "index.m3u8" {
		query := ""
		if options.Height > 0 {
			query = "?" + url.Values{"height": {strconv.Itoa(options.Height)}}.Encode()
		}
		if transcode.SegmentCount(probe) == 0 {
			h.writeError(w, http.StatusUnprocessableEntity, "unknown_duration", "Media duration is unknown")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = w.Write([]byte(transcode.BuildPlaylist(probe, query)))
		return
	}

	indexText, isSegment := strings.CutSuffix(file, ".ts")
	index, err := strconv.Atoi(indexText)
	if !isSegment || err != nil {
		h.writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	plan, err := transcode.BuildSegmentPlan(path, probe, index, options.Height, h.transcoder.Settings())
	if err != nil {
		h.writeError(w, http.StatusNotFound, "not_found", "Segment is out of range")
		return
	}
	plan.Meta = sessionMeta(r, claims, locator, probe, "hls")
	h.finishStream(w, r, plan, transcode.ModeTranscode, "2")
}
