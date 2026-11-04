package transcode

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildPlaylist(t *testing.T) {
	playlist := BuildPlaylist(Probe{Duration: 14}, "?audioTrack=3&height=720", "")
	if got := strings.Count(playlist, "#EXTINF"); got != 3 {
		t.Fatalf("segments = %d, want 3", got)
	}
	if !strings.Contains(playlist, "#EXTINF:2.000,\n2.ts?audioTrack=3&height=720") {
		t.Errorf("last segment wrong:\n%s", playlist)
	}
	if !strings.HasSuffix(playlist, "#EXT-X-ENDLIST\n") {
		t.Error("playlist must end with ENDLIST")
	}
}

func TestBuildSegmentPlan(t *testing.T) {
	probe := Probe{VideoCodec: "hevc", Height: 1080, Duration: 30}
	plan, err := BuildSegmentPlan("a.mkv", probe, 2, Options{Height: 720})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ContentType != "video/mp2t" || !slices.Contains(plan.Args, "12.000") || !slices.Contains(plan.Args, "scale=-2:720") {
		t.Errorf("unexpected plan: %v", plan.Args)
	}
	if _, err := BuildSegmentPlan("a.mkv", probe, 5, Options{}); err == nil {
		t.Error("expected out-of-range segment to fail")
	}
}

func TestBuildSegmentPlanMapsSelectedAudioStream(t *testing.T) {
	probe := Probe{VideoCodec: "hevc", Height: 1080, Duration: 30, AudioTracks: []AudioTrack{{Index: 2, Codec: "aac"}}}
	plan, err := BuildSegmentPlan("a.mkv", probe, 0, Options{AudioStreamIndex: intPointer(2)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Args, "0:2") {
		t.Fatalf("selected audio stream was not mapped: %v", plan.Args)
	}
}

func TestBuildPlaylistAdvertisesKey(t *testing.T) {
	playlist := BuildPlaylist(Probe{Duration: 14}, "", "key")
	if !strings.Contains(playlist, `#EXT-X-KEY:METHOD=AES-128,URI="key"`) {
		t.Fatalf("playlist has no key tag:\n%s", playlist)
	}
}

func TestBuildAudioSegmentPlan(t *testing.T) {
	probe := Probe{Duration: 14}
	plan, err := BuildAudioSegmentPlan("/music/a.flac", probe, 1)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.Args, " ")
	for _, want := range []string{"-ss 6.000", "-c:a aac", "-vn", "-f mpegts"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if _, err := BuildAudioSegmentPlan("/music/a.flac", probe, 3); err == nil {
		t.Error("segment past the end must fail")
	}
}
