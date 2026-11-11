package httpserver

import (
	"fmt"
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
	locator, claims, ok := h.authorizeToken(w, r, rawToken)
	if !ok {
		return
	}
	key := transcode.HLSKey(h.secret, claims.PlaySessionID, claims.MediaItemID)
	if file == "key" {
		// The key is only released to a holder of a valid token, so saved segments cannot be decrypted later.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = w.Write(key)
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
	if !isAudioFile(locator.RelativePath) {
		options = h.pluginOptions(r.Context(), claims, options)
	}
	path, probe, _, ok := h.prepare(w, r, locator, options)
	if !ok {
		return
	}

	if file == "index.m3u8" {
		queryValues := url.Values{}
		if options.Height > 0 {
			queryValues.Set("height", strconv.Itoa(options.Height))
		}
		if options.AudioStreamIndex != nil {
			queryValues.Set("audioTrack", strconv.Itoa(*options.AudioStreamIndex))
		}
		query := ""
		if encoded := queryValues.Encode(); encoded != "" {
			query = "?" + encoded
		}
		if transcode.SegmentCount(probe) == 0 {
			h.writeError(w, http.StatusUnprocessableEntity, "unknown_duration", "Media duration is unknown")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "private, no-store")
		playlist := transcode.BuildPlaylist(probe, query, "key")
		result := h.pluginHook(r.Context(), "manifest", claims, options.Height, probe.Duration)
		playlist = applyManifestStart(playlist, result.StartSeconds, probe.Duration)
		_, _ = w.Write([]byte(playlist))
		return
	}

	indexText, isSegment := strings.CutSuffix(file, ".ts")
	index, err := strconv.Atoi(indexText)
	if !isSegment || err != nil {
		h.writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	options.Encoder = h.transcoder.Settings()
	var plan transcode.Plan
	if isAudioFile(locator.RelativePath) {
		plan, err = transcode.BuildAudioSegmentPlan(path, probe, index)
	} else {
		plan, err = transcode.BuildSegmentPlan(path, probe, index, options)
	}
	if err != nil {
		h.writeError(w, http.StatusNotFound, "not_found", "Segment is out of range")
		return
	}
	plan.Encrypt = &transcode.Encryption{Key: key, IV: transcode.HLSIV(index)}
	plan.Meta = sessionMeta(r, claims, locator, probe, "hls")
	h.finishStream(w, r, plan, transcode.ModeTranscode, "2")
}

func applyManifestStart(playlist string, start *float64, duration float64) string {
	if start == nil || *start < 0 || *start > duration {
		return playlist
	}
	return strings.Replace(playlist, "#EXTM3U\n", fmt.Sprintf("#EXTM3U\n#EXT-X-START:TIME-OFFSET=%.3f,PRECISE=YES\n", *start), 1)
}
