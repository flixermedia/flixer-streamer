package transcode

import (
	"errors"
	"slices"
	"testing"
)

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
}
