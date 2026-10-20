package monitoring

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFetchReturnsConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sentry/config/streamer" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"enabled":true,"dsn":" https://key@example.com/1 "}`))
	}))
	defer server.Close()

	cfg := Fetch(context.Background(), FetchOptions{BaseURL: server.URL + "/", App: "streamer", Logger: quietLogger()})
	if !cfg.Enabled || cfg.DSN != "https://key@example.com/1" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestFetchRetriesUntilFlixerResponds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"enabled":false,"dsn":null}`))
	}))
	defer server.Close()

	cfg := Fetch(context.Background(), FetchOptions{
		BaseURL: server.URL, App: "streamer", Interval: 5 * time.Millisecond, Logger: quietLogger(),
	})
	if cfg.Enabled || calls.Load() != 3 {
		t.Fatalf("expected disabled config after 3 calls, got %+v after %d", cfg, calls.Load())
	}
}

func TestFetchDisablesWhenUnreachable(t *testing.T) {
	cfg := Fetch(context.Background(), FetchOptions{
		BaseURL: "http://127.0.0.1:1", App: "streamer",
		Timeout: 100 * time.Millisecond, Interval: 10 * time.Millisecond, Logger: quietLogger(),
	})
	if cfg.Enabled || cfg.DSN != "" {
		t.Fatalf("expected disabled config, got %+v", cfg)
	}
}

func TestStartDisabled(t *testing.T) {
	enabled, flush, err := Start(Config{Enabled: false, DSN: "https://key@example.com/1"}, "")
	if err != nil || enabled {
		t.Fatalf("expected disabled without error, got enabled=%v err=%v", enabled, err)
	}
	flush()
}
