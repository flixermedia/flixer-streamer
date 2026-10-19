package transcode

import (
	"errors"
	"testing"
)

func TestRegistryListAndCancel(t *testing.T) {
	reg := newRegistry()
	cancelled := false
	meta := SessionMeta{ID: "play-1", UserID: "u1", File: "a.mkv", Delivery: "progressive"}

	run, err := reg.begin(meta, ModeTranscode, 720, 30, func() { cancelled = true })
	if err != nil {
		t.Fatal(err)
	}
	run.setPID(42)
	run.addBytes(1000)

	sessions := reg.list()
	if len(sessions) != 1 || !sessions[0].Running || sessions[0].BytesSent != 1000 || sessions[0].PID != 42 || sessions[0].Height != 720 {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}

	if !reg.cancel("play-1") || !cancelled || !run.cancelled() {
		t.Fatal("cancel should stop the running ffmpeg")
	}
	if reg.cancel("play-1") {
		t.Error("cancelling an unknown session should report false")
	}
	if len(reg.list()) != 0 {
		t.Error("cancelled session should be removed")
	}
	if _, err := reg.begin(meta, ModeTranscode, 720, 0, func() {}); !errors.Is(err, ErrCancelled) {
		t.Errorf("revoked session restarted: %v", err)
	}
}

func TestRegistryKeepsIdleSessionBriefly(t *testing.T) {
	reg := newRegistry()
	run, err := reg.begin(SessionMeta{ID: "hls-1", Delivery: "hls"}, ModeTranscode, 0, 0, func() {})
	if err != nil {
		t.Fatal(err)
	}
	run.end()
	sessions := reg.list()
	if len(sessions) != 1 || sessions[0].Running {
		t.Fatalf("idle session should stay listed but not running: %+v", sessions)
	}
}
