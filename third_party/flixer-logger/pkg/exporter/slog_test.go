package exporter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type collectingSink struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (s *collectingSink) Enqueue(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, event)
	return nil
}

func TestSlogHandlerPreservesBaseAndRedactsGroups(t *testing.T) {
	var output bytes.Buffer
	base := slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})
	sink := &collectingSink{}
	var failures []error
	handler, err := NewSlogHandler(base, sink, HandlerOptions{
		App: "indexer", InstanceID: "one", Source: "pipeline",
		OnError: func(err error) { failures = append(failures, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(handler).With("task_id", "123", "task_queue", "scan", "api_key", "private-key").
		WithGroup("request").With("url", "https://private.example/path")
	logger.Debug("not enabled")
	logger.Info("password=secret", "detail", "alice@example.com", "count", 3)
	if len(failures) != 0 || len(sink.events) != 1 {
		t.Fatalf("events=%d failures=%v", len(sink.events), failures)
	}
	event := sink.events[0]
	if event.EventID == "" || event.Level != "info" || event.App != "indexer" ||
		event.TaskID != "123" || event.Fields["task_id"] != "123" || event.Fields["task_queue"] != "scan" || event.Fields["api_key"] != "[REDACTED]" ||
		strings.Contains(event.Message, "secret") {
		t.Fatalf("unexpected event: %+v", event)
	}
	group, ok := event.Fields["request"].(map[string]any)
	if !ok || group["count"] != float64(3) ||
		strings.Contains(group["url"].(string), "private.example") ||
		strings.Contains(group["detail"].(string), "alice@example.com") {
		t.Fatalf("unexpected group: %#v", event.Fields["request"])
	}
	if !strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), "private.example") {
		t.Fatalf("original base handler was altered: %s", output.String())
	}
}

func TestSlogHandlerReportsExporterFailureWithoutFailingBase(t *testing.T) {
	var output bytes.Buffer
	sink := &collectingSink{err: ErrQueueFull}
	var failure error
	handler, err := NewSlogHandler(slog.NewJSONHandler(&output, nil), sink,
		HandlerOptions{App: "api", InstanceID: "one", OnError: func(err error) { failure = err }})
	if err != nil {
		t.Fatal(err)
	}
	record := slog.NewRecord(testEvent("id").Time, slog.LevelWarn, "warning", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(failure, ErrQueueFull) || !strings.Contains(output.String(), "warning") {
		t.Fatalf("failure=%v output=%s", failure, output.String())
	}
}

func TestSlogHandlerConcurrentWrites(t *testing.T) {
	sink := &collectingSink{}
	handler, err := NewSlogHandler(slog.NewJSONHandler(&lockedBuffer{}, nil), sink,
		HandlerOptions{App: "api", InstanceID: "one", OnError: func(err error) { t.Error(err) }})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(handler).With("source", "test")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); logger.Info("message") }()
	}
	wg.Wait()
	if len(sink.events) != 50 {
		t.Fatalf("got %d events", len(sink.events))
	}
	ids := make(map[string]bool)
	for _, event := range sink.events {
		if ids[event.EventID] {
			t.Fatal("duplicate event ID")
		}
		ids[event.EventID] = true
	}
}

type lockedBuffer struct {
	mu sync.Mutex
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(p), nil
}
