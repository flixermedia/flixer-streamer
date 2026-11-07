package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagedCredentialChangesDiscardQueuedEvents(t *testing.T) {
	first, second := strings.Repeat("x", 43), strings.Repeat("y", 43)
	type batch struct {
		token  string
		events []Event
	}
	batches := make(chan batch, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Events []Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		batches <- batch{r.Header.Get("Authorization"), payload.Events}
		fmt.Fprintf(w, `{"accepted":%d}`, len(payload.Events))
	}))
	defer server.Close()
	credential := Credential{Endpoint: server.URL, Token: first, ExpiresAt: time.Now().Add(time.Hour)}
	reports := make(chan error, 2)
	sink := &ManagedSink{config: ManagedConfig{
		Fetch: func(context.Context) (Credential, error) { return credential, nil },
		Exporter: Config{QueueSize: 10, BatchSize: 10, FlushInterval: time.Hour,
			Timeout: time.Second, RetryDelay: time.Millisecond, MaxAttempts: 1, OnError: func(err error) { reports <- err }},
	}}
	sink.refresh(context.Background())
	old := sink.current
	if err := sink.Enqueue(testEvent("old")); err != nil {
		t.Fatal(err)
	}
	credential.Token = second
	sink.refresh(context.Background())
	if err := sink.Enqueue(testEvent("new")); err != nil {
		t.Fatal(err)
	}
	sink.replace(nil, Credential{}, false)
	if old.Dropped() != 1 {
		t.Fatalf("expected one discarded old event, got %d", old.Dropped())
	}
	select {
	case report := <-reports:
		if !strings.Contains(report.Error(), "1 events discarded") {
			t.Fatalf("unexpected diagnostic: %v", report)
		}
	default:
		t.Fatal("discard was not reported")
	}
	select {
	case got := <-batches:
		if got.token != "Bearer "+second || len(got.events) != 1 || got.events[0].EventID != "new" {
			t.Fatalf("unexpected batch: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected new event was not delivered")
	}
	select {
	case got := <-batches:
		t.Fatalf("delivered a discarded event: %+v", got)
	default:
	}
}

func TestManagedFailureCancelsInFlightDelivery(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
			t.Error("server did not observe cancellation")
		}
	}))
	defer server.Close()
	unavailable := false
	sink := &ManagedSink{config: ManagedConfig{
		Fetch: func(context.Context) (Credential, error) {
			if unavailable {
				return Credential{}, ErrConfigurationUnavailable
			}
			return Credential{Endpoint: server.URL, Token: strings.Repeat("x", 43), ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
		Exporter: Config{QueueSize: 10, BatchSize: 1, FlushInterval: time.Hour,
			Timeout: 10 * time.Second, RetryDelay: time.Second, MaxAttempts: 3, OnError: func(error) {}},
	}}
	sink.refresh(context.Background())
	if err := sink.Enqueue(testEvent("in-flight")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery did not start")
	}
	unavailable = true
	sink.refresh(context.Background())
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request was not cancelled")
	}
	if err := sink.Enqueue(testEvent("after-failure")); err != ErrConfigurationUnavailable {
		t.Fatalf("delivery was not suspended: %v", err)
	}
}
