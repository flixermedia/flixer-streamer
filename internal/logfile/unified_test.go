package logfile

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestUnifiedAttrsProducesSharedSchema(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: UnifiedAttrs})).With("app", "streamer")
	logger.Warn("disk low", "source", "ffmpeg", "pid", 7)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"level": "warn", "message": "disk low", "app": "streamer", "source": "ffmpeg", "pid": float64(7)}
	for key, value := range want {
		if record[key] != value {
			t.Errorf("%s = %v, want %v", key, record[key], value)
		}
	}
	if _, ok := record["msg"]; ok {
		t.Error("msg key should be renamed to message")
	}
	if ts, _ := record["time"].(string); ts == "" || ts[len(ts)-1] != 'Z' {
		t.Errorf("time should be UTC RFC 3339, got %v", record["time"])
	}
}
