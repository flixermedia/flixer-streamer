package transcode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDefaultSettingsKeepSoftwareEncoder(t *testing.T) {
	got := strings.Join(Settings{}.videoArgs(), " ")
	want := "-c:v libx264 -preset veryfast -crf 23 -profile:v high -pix_fmt yuv420p"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSettingsAreApplied(t *testing.T) {
	args := Settings{Preset: "slow", CRF: 28, Threads: 4, MaxBitrateKbps: 8000}.videoArgs()
	for _, pair := range [][]string{{"-preset", "slow"}, {"-crf", "28"}, {"-threads", "4"}, {"-maxrate", "8000k"}, {"-bufsize", "16000k"}} {
		i := slices.Index(args, pair[0])
		if i < 0 || args[i+1] != pair[1] {
			t.Fatalf("missing %v in %v", pair, args)
		}
	}
}

func TestHardwareEncoders(t *testing.T) {
	for accel, encoder := range map[string]string{"nvenc": "h264_nvenc", "qsv": "h264_qsv", "vaapi": "h264_vaapi", "amf": "h264_amf"} {
		args := Settings{HWAccel: accel}.videoArgs()
		if !slices.Contains(args, encoder) {
			t.Errorf("%s: %v lacks %s", accel, args, encoder)
		}
	}
	if got := (Settings{HWAccel: "vaapi"}).videoFilter(720); got != "scale=-2:720,format=nv12,hwupload" {
		t.Errorf("vaapi filter = %q", got)
	}
	if got := (Settings{HWAccel: "vaapi"}).inputArgs(); len(got) != 2 {
		t.Errorf("vaapi input args = %v", got)
	}
	if got := (Settings{}).inputArgs(); got != nil {
		t.Errorf("software input args = %v", got)
	}
}

func TestInvalidSettingsFallBack(t *testing.T) {
	s := Settings{HWAccel: "bogus", Preset: "nope", CRF: 99, Threads: -1, MaxBitrateKbps: -5}.resolved()
	if s.HWAccel != "none" || s.Preset != "veryfast" || s.CRF != 23 || s.Threads != 0 || s.MaxBitrateKbps != 0 {
		t.Fatalf("unexpected %+v", s)
	}
}

func TestLimiterChangesAtRuntime(t *testing.T) {
	var l limiter
	l.setLimit(1)
	if err := l.acquire(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := l.acquire(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- l.acquire(context.Background(), 2*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	l.setLimit(2)
	if err := <-done; err != nil {
		t.Fatalf("raising the limit should admit the waiter: %v", err)
	}
	l.setLimit(0)
	if err := l.acquire(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("0 means unlimited: %v", err)
	}
}
