package transcode

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func intPointer(value int) *int { return &value }

func TestDecide(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		probe   Probe
		options Options
		want    Mode
	}{
		{"mp4 h264 direct", "a.mp4", Probe{VideoCodec: "h264", Height: 1080}, Options{}, ModeDirectPlay},
		{"mkv h264 remux", "a.mkv", Probe{VideoCodec: "h264", Height: 1080}, Options{}, ModeRemux},
		{"mkv vp9 remux", "a.mkv", Probe{VideoCodec: "vp9", Height: 720}, Options{}, ModeRemux},
		{"hevc transcodes", "a.mkv", Probe{VideoCodec: "hevc", Height: 1080}, Options{}, ModeTranscode},
		{"smaller height transcodes", "a.mp4", Probe{VideoCodec: "h264", Height: 1080}, Options{Height: 480}, ModeTranscode},
		{"no upscaling", "a.mp4", Probe{VideoCodec: "h264", Height: 720}, Options{Height: 1080}, ModeDirectPlay},
		{"same height", "a.mkv", Probe{VideoCodec: "h264", Height: 720}, Options{Height: 720}, ModeRemux},
		{"start offset needs ffmpeg", "a.mp4", Probe{VideoCodec: "h264", Height: 720}, Options{StartSeconds: 4}, ModeRemux},
		{"audio selection needs ffmpeg", "a.mp4", Probe{VideoCodec: "h264", Height: 720}, Options{AudioStreamIndex: intPointer(2)}, ModeRemux},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Decide(test.file, test.probe, test.options); got != test.want {
				t.Fatalf("Decide() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestBuildPlanRemuxH264ToFragmentedMP4(t *testing.T) {
	plan, err := BuildPlan("in.mkv", Probe{VideoCodec: "h264", AudioCodec: "aac"}, ModeRemux, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ContentType != "video/mp4" || !slices.Contains(plan.Args, "frag_keyframe+empty_moov+default_base_moof") {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if !slices.Contains(plan.Args, "copy") || slices.Contains(plan.Args, "libx264") {
		t.Fatalf("expected stream copy, got %v", plan.Args)
	}
}

func TestBuildPlanRemuxVP9ToWebM(t *testing.T) {
	plan, err := BuildPlan("in.mkv", Probe{VideoCodec: "vp9", AudioCodec: "opus"}, ModeRemux, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ContentType != "video/webm" || !slices.Contains(plan.Args, "webm") {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestBuildPlanTranscodeScalesAndSeeks(t *testing.T) {
	plan, err := BuildPlan("in.mkv", Probe{VideoCodec: "h264", Height: 1080}, ModeTranscode, Options{Height: 480, StartSeconds: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"libx264", "scale=-2:480", "-ss", "2.500", "aac"} {
		if !slices.Contains(plan.Args, want) {
			t.Fatalf("missing %q in %v", want, plan.Args)
		}
	}
	if slices.Index(plan.Args, "-ss") > slices.Index(plan.Args, "-i") {
		t.Fatal("-ss must precede -i for fast seeking")
	}
}

func TestBuildPlanRejectsInvalidOptions(t *testing.T) {
	if _, err := BuildPlan("in.mkv", Probe{VideoCodec: "h264"}, ModeTranscode, Options{Height: 999}); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("expected ErrInvalidArgs, got %v", err)
	}
	if _, err := BuildPlan("in.mp4", Probe{VideoCodec: "h264"}, ModeDirectPlay, Options{}); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("expected ErrInvalidArgs, got %v", err)
	}
	if _, err := BuildPlan("in.mp4", Probe{VideoCodec: "h264"}, ModeRemux, Options{AudioStreamIndex: intPointer(2)}); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("expected invalid audio track to return ErrInvalidArgs, got %v", err)
	}
}

func TestBuildAudioPlanProducesMPEG1Layer3CBR(t *testing.T) {
	plan, err := BuildAudioPlan("in.flac", 320, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != ModeTranscode || plan.ContentType != "audio/mpeg" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, want := range []string{"libmp3lame", "320k", "-ar", "44100", "-ac", "2", "-f", "mp3", "pipe:1"} {
		if !slices.Contains(plan.Args, want) {
			t.Errorf("missing %q in %v", want, plan.Args)
		}
	}
	if !slices.Contains(plan.Args, "-minrate") || !slices.Contains(plan.Args, "-maxrate") {
		t.Fatalf("MP3 plan does not set constant bitrate bounds: %v", plan.Args)
	}
}

func TestBuildAudioPlanSeeksBeforeTheInput(t *testing.T) {
	plan, err := BuildAudioPlan("in.flac", 192, 83.5)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.Args, " ")
	if !strings.Contains(args, "-ss 83.500 -i in.flac") {
		t.Fatalf("args = %s", args)
	}
	if plan.Position != 83.5 {
		t.Fatalf("position = %v", plan.Position)
	}
}

func TestBuildAudioPlanRejectsUnsupportedBitrate(t *testing.T) {
	for _, bitrate := range []int{0, 127, 129, 321} {
		if _, err := BuildAudioPlan("in.flac", bitrate, 0); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("bitrate %d: expected ErrInvalidArgs, got %v", bitrate, err)
		}
	}
}

func TestBuildPlanMapsSelectedAudioStream(t *testing.T) {
	probe := Probe{
		VideoCodec: "h264",
		AudioTracks: []AudioTrack{
			{Index: 1, Codec: "aac"},
			{Index: 3, Codec: "ac3"},
		},
	}
	plan, err := BuildPlan("in.mkv", probe, ModeRemux, Options{AudioStreamIndex: intPointer(3)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Args, "0:3") || !slices.Contains(plan.Args, "aac") {
		t.Fatalf("selected audio stream was not mapped and encoded: %v", plan.Args)
	}
}

func TestParseProbeOutputIncludesAudioMetadata(t *testing.T) {
	probe, err := parseProbeOutput([]byte(`{
		"streams": [
			{"index": 0, "codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
			{"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "eng", "title": "Stereo"}, "disposition": {"default": 1}},
			{"index": 2, "codec_type": "audio", "codec_name": "ac3", "channels": 6, "tags": {"language": "jpn"}, "disposition": {"default": 0}}
		],
		"format": {"duration": "90.5"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if probe.AudioCodec != "aac" || len(probe.AudioTracks) != 2 || probe.AudioTracks[1].Index != 2 || probe.AudioTracks[0].Language != "eng" || !probe.AudioTracks[0].Default || probe.AudioTracks[1].Channels != 6 {
		t.Fatalf("unexpected probe: %+v", probe)
	}
}
