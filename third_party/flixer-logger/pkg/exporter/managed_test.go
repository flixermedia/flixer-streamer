package exporter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestManagedSinkSuspendsAndRecovers(t *testing.T) {
	var mu sync.Mutex
	credential := Credential{Endpoint: "http://127.0.0.1:1", Token: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", ExpiresAt: time.Now().Add(time.Hour)}
	unavailable := false
	fetch := func(context.Context) (Credential, error) {
		mu.Lock()
		defer mu.Unlock()
		if unavailable {
			return Credential{}, errors.New("unavailable")
		}

		return credential, nil
	}
	config := Config{
		QueueSize: 10, BatchSize: 10, FlushInterval: time.Hour,
		Timeout: time.Millisecond * 10, RetryDelay: time.Millisecond, MaxAttempts: 1,
		OnError: func(error) {},
	}
	sink, err := NewManaged(ManagedConfig{Fetch: fetch, RefreshInterval: time.Second, Exporter: config})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close(context.Background())
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for managed configuration")
	}
	wait(func() bool { sink.mu.RLock(); defer sink.mu.RUnlock(); return sink.current != nil })
	mu.Lock()
	unavailable = true
	mu.Unlock()
	wait(func() bool { sink.mu.RLock(); defer sink.mu.RUnlock(); return sink.current == nil })
	if err := sink.Enqueue(Event{}); !errors.Is(err, ErrConfigurationUnavailable) {
		t.Fatalf("expected suspended export, got %v", err)
	}
	mu.Lock()
	unavailable = false
	credential.Token = "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	mu.Unlock()
	wait(func() bool {
		sink.mu.RLock()
		defer sink.mu.RUnlock()
		return sink.current != nil && sink.active.Token == credential.Token
	})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Enqueue(Event{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected closed exporter, got %v", err)
	}
}
