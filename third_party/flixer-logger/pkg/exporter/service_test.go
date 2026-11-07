package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServiceEnrolsAndPreservesBaseLogging(t *testing.T) {
	var endpoint string
	received := make(chan Event, 1)
	token := strings.Repeat("x", 43)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/internal/logger/streamer":
			json.NewEncoder(w).Encode(map[string]any{"endpoint": endpoint, "token": token, "expiresAt": time.Now().Add(time.Hour)})
		case "/api/logs/batch":
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("wrong credential")
			}
			var body struct {
				Events []Event `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			for _, event := range body.Events {
				received <- event
			}
			fmt.Fprintf(w, `{"accepted":%d}`, len(body.Events))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	endpoint = server.URL
	var base bytes.Buffer
	handler, service, err := StartService(slog.NewJSONHandler(&base, nil), server.URL, "flixer-streamer", []byte(strings.Repeat("s", 32)), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for {
		service.Sink.mu.RLock()
		ready := service.Sink.current != nil
		service.Sink.mu.RUnlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("enrollment did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	slog.New(handler).Info("playback", "token", "do-not-export")
	var instance string
	select {
	case event := <-received:
		if event.App != "flixer-streamer" || event.Message != "playback" || event.Fields["token"] != "[REDACTED]" {
			t.Fatalf("unexpected event: %+v", event)
		}
		instance = event.InstanceID
	case <-time.After(3 * time.Second):
		t.Fatal("event was not delivered")
	}
	if !strings.Contains(base.String(), "playback") {
		t.Fatal("base handler was not called")
	}
	slog.New(service.TaskHandler()).With("task_id", "task", "task_queue", "scan").Info("task progress")
	select {
	case event := <-received:
		if event.Message != "task progress" || event.InstanceID != instance || event.TaskID != "task" || event.Fields["task_queue"] != "scan" {
			t.Fatalf("unexpected task event: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task event was not delivered")
	}
	if strings.Contains(base.String(), "task progress") {
		t.Fatal("task-only fallback changed base output")
	}
}
