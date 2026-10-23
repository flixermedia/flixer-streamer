package transcode

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildPlaylist(t *testing.T) {
	playlist := BuildPlaylist(Probe{Duration: 14}, "?height=720")
	if got := strings.Count(playlist, "#EXTINF"); got != 3 {
		t.Fatalf("segments = %d, want 3", got)
	}
	if !strings.Contains(playlist, "#EXTINF:2.000,\n2.ts?height=720") {
		t.Errorf("last segment wrong:\n%s", playlist)
	}
	if !strings.HasSuffix(playlist, "#EXT-X-ENDLIST\n") {
		t.Error("playlist must end with ENDLIST")
	}
}

func TestBuildSegmentPlan(t *testing.T) {
	probe := Probe{VideoCodec: "hevc", Height: 1080, Duration: 30}
	plan, err := BuildSegmentPlan("a.mkv", probe, 2, 720, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ContentType != "video/mp2t" || !slices.Contains(plan.Args, "12.000") || !slices.Contains(plan.Args, "scale=-2:720") {
		t.Errorf("unexpected plan: %v", plan.Args)
	}
	if _, err := BuildSegmentPlan("a.mkv", probe, 5, 0, Settings{}); err == nil {
		t.Error("expected out-of-range segment to fail")
	}
}
