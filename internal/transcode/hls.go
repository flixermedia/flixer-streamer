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
// so the selected height and audio track carry over.
func BuildPlaylist(probe Probe, query, keyURI string) string {
	count := SegmentCount(probe)
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", SegmentSeconds)
	if keyURI != "" {
		fmt.Fprintf(&b, "#EXT-X-KEY:METHOD=AES-128,URI=%q\n", keyURI)
	}
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

// AudioHLSBitrateKbps is the AAC bitrate of encrypted audio segments.
const AudioHLSBitrateKbps = 256

// BuildAudioSegmentPlan returns ffmpeg arguments that encode one AAC MPEG-TS segment of an audio file.
func BuildAudioSegmentPlan(inputPath string, probe Probe, index int) (Plan, error) {
	if index < 0 || index >= SegmentCount(probe) {
		return Plan{}, ErrInvalidArgs
	}
	start := strconv.FormatFloat(float64(index*SegmentSeconds), 'f', 3, 64)
	return Plan{
		Mode:        ModeTranscode,
		ContentType: "video/mp2t",
		Position:    float64(index * SegmentSeconds),
		Args: []string{
			"-hide_banner", "-nostdin",
			"-ss", start, "-i", inputPath, "-t", strconv.Itoa(SegmentSeconds),
			"-map", "0:a:0", "-vn", "-sn", "-dn",
			"-c:a", "aac", "-b:a", strconv.Itoa(AudioHLSBitrateKbps) + "k", "-ar", "44100", "-ac", "2",
			"-output_ts_offset", start, "-muxdelay", "0",
			"-f", "mpegts", "pipe:1",
		},
	}, nil
}

// BuildSegmentPlan returns ffmpeg arguments that encode one H.264/AAC MPEG-TS
// segment. Video is always re-encoded so every segment starts on a keyframe,
// and timestamps are offset so segments join seamlessly.
func BuildSegmentPlan(inputPath string, probe Probe, index int, options Options) (Plan, error) {
	if !ValidHeight(options.Height) || index < 0 || index >= SegmentCount(probe) {
		return Plan{}, ErrInvalidArgs
	}
	if options.AudioStreamIndex != nil && !probe.HasAudioTrack(*options.AudioStreamIndex) {
		return Plan{}, ErrInvalidArgs
	}
	start := strconv.FormatFloat(float64(index*SegmentSeconds), 'f', 3, 64)
	args := append([]string{"-hide_banner", "-nostdin"}, options.Encoder.inputArgs()...)
	audioMap := "0:a:0?"
	if options.AudioStreamIndex != nil {
		audioMap = "0:" + strconv.Itoa(*options.AudioStreamIndex)
	}
	args = append(args,
		"-ss", start, "-i", inputPath, "-t", strconv.Itoa(SegmentSeconds),
		"-map", "0:v:0", "-map", audioMap, "-sn", "-dn",
	)
	args = append(args, options.Encoder.videoArgs()...)
	args = append(args, "-force_key_frames", "0")
	scaleHeight := 0
	if needsScaling(probe, options) {
		scaleHeight = options.Height
	}
	if filter := options.Encoder.videoFilter(scaleHeight); filter != "" {
		args = append(args, "-vf", filter)
	}
	args = append(args,
		"-c:a", "aac", "-b:a", "160k", "-ac", "2",
		"-output_ts_offset", start, "-muxdelay", "0",
		"-f", "mpegts", "pipe:1",
	)
	return Plan{Mode: ModeTranscode, ContentType: "video/mp2t", Args: args, Height: options.Height, Position: float64(index * SegmentSeconds)}, nil
}
