package transcode

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// SegmentSeconds is the fixed HLS segment length. Segments are encoded on
// demand, so the playlist is a static VOD list derived from the probed duration.
const SegmentSeconds = 6

// SegmentCount returns how many segments cover the probed duration.
func SegmentCount(probe Probe) int {
	if probe.Duration <= 0 {
		return 0
	}
	return int(math.Ceil(probe.Duration / SegmentSeconds))
}

// BuildPlaylist renders the VOD playlist. query is appended to each segment URI
// so the selected height carries over.
func BuildPlaylist(probe Probe, query string) string {
	count := SegmentCount(probe)
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", SegmentSeconds)
	for i := 0; i < count; i++ {
		length := float64(SegmentSeconds)
		if i == count-1 {
			if rest := probe.Duration - float64(i*SegmentSeconds); rest > 0 {
				length = rest
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%d.ts%s\n", length, i, query)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// BuildSegmentPlan returns ffmpeg arguments that encode one H.264/AAC MPEG-TS
// segment. Video is always re-encoded so every segment starts on a keyframe,
// and timestamps are offset so segments join seamlessly.
func BuildSegmentPlan(inputPath string, probe Probe, index int, height int, encoder Settings) (Plan, error) {
	if !ValidHeight(height) || index < 0 || index >= SegmentCount(probe) {
		return Plan{}, ErrInvalidArgs
	}
	start := strconv.FormatFloat(float64(index*SegmentSeconds), 'f', 3, 64)
	args := append([]string{"-hide_banner", "-nostdin"}, encoder.inputArgs()...)
	args = append(args,
		"-ss", start, "-i", inputPath, "-t", strconv.Itoa(SegmentSeconds),
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
	)
	args = append(args, encoder.videoArgs()...)
	args = append(args, "-force_key_frames", "0")
	scaleHeight := 0
	if needsScaling(probe, Options{Height: height}) {
		scaleHeight = height
	}
	if filter := encoder.videoFilter(scaleHeight); filter != "" {
		args = append(args, "-vf", filter)
	}
	args = append(args,
		"-c:a", "aac", "-b:a", "160k", "-ac", "2",
		"-output_ts_offset", start, "-muxdelay", "0",
		"-f", "mpegts", "pipe:1",
	)
	return Plan{Mode: ModeTranscode, ContentType: "video/mp2t", Args: args, Height: height, Position: float64(index * SegmentSeconds)}, nil
}
