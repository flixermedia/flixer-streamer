package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(endpoint string, onError func(error)) Config {
	return Config{Endpoint: endpoint, Token: strings.Repeat("x", 32), QueueSize: 10, BatchSize: 2,
		FlushInterval: 10 * time.Millisecond, Timeout: time.Second, RetryDelay: time.Millisecond,
		MaxAttempts: 2, OnError: onError}
}

func testEvent(id string) Event {
	return Event{SchemaVersion: 1, EventID: id, Time: time.Now(), Level: "info", InstanceID: "one", App: "api", Message: "ready"}
}

func TestRetryPreservesIDsAndFlushes(t *testing.T) {
	var mu sync.Mutex
	var requests [][]Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/logs/batch" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) ||
			r.Header.Get("X-Flixer-Log-Schema") != "1" {
			t.Error("unexpected request")
		}
		var batch struct {
			Events []Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, batch.Events)
		attempt := len(requests)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"accepted":%d}`, len(batch.Events))
	}))
	defer server.Close()
	e, err := New(testConfig(server.URL, func(err error) { t.Error(err) }))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if err := e.Enqueue(testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || len(requests[0]) != 2 || len(requests[1]) != 2 ||
		requests[0][0].EventID != requests[1][0].EventID || requests[0][1].EventID != requests[1][1].EventID || e.Dropped() != 0 {
		t.Fatalf("unexpected retries: %+v, dropped %d", requests, e.Dropped())
	}
	if !errors.Is(e.Enqueue(testEvent("late")), ErrClosed) {
		t.Fatal("event accepted after close")
	}
}

func TestRotateTokenDuringRetry(t *testing.T) {
	first := make(chan string, 1)
	second := make(chan string, 1)
	release := make(chan struct{})
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			first <- r.Header.Get("Authorization")
			<-release
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		second <- r.Header.Get("Authorization")
		fmt.Fprint(w, `{"accepted":1}`)
	}))
	defer server.Close()
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRequest()
	config := testConfig(server.URL, func(err error) { t.Error(err) })
	config.BatchSize = 1
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(testEvent("rotate")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-first:
		if got != "Bearer "+strings.Repeat("x", 32) {
			t.Fatal("first request used an unexpected credential")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not start")
	}
	if err := e.RotateToken("short"); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if err := e.RotateToken(strings.Repeat("y", 32)); err != nil {
		t.Fatal(err)
	}
	releaseRequest()
	select {
	case got := <-second:
		if got != "Bearer "+strings.Repeat("y", 32) {
			t.Fatal("retry did not use replacement credential")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.RotateToken(strings.Repeat("z", 32)); !errors.Is(err, ErrClosed) {
		t.Fatalf("rotation after close: %v", err)
	}
	if e.Dropped() != 0 {
		t.Fatalf("unexpected dropped events: %d", e.Dropped())
	}
}

func TestRotateTokenAfterInFlightUnauthorized(t *testing.T) {
	first, second := make(chan string, 1), make(chan string, 1)
	release := make(chan struct{})
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			first <- r.Header.Get("Authorization")
			<-release
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		second <- r.Header.Get("Authorization")
		fmt.Fprint(w, `{"accepted":1}`)
	}))
	defer server.Close()
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRequest()
	config := testConfig(server.URL, func(err error) { t.Error(err) })
	config.BatchSize = 1
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(testEvent("rotate-unauthorized")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-first:
		if got != "Bearer "+strings.Repeat("x", 32) {
			t.Fatal("first request used an unexpected credential")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not start")
	}
	if err := e.RotateToken(strings.Repeat("y", 32)); err != nil {
		t.Fatal(err)
	}
	releaseRequest()
	select {
	case got := <-second:
		if got != "Bearer "+strings.Repeat("y", 32) {
			t.Fatal("retry did not use replacement credential")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if e.Dropped() != 0 {
		t.Fatalf("unexpected dropped events: %d", e.Dropped())
	}
}

func TestUnauthorizedWithoutRotationIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	reported := make(chan error, 1)
	config := testConfig(server.URL, func(err error) { reported <- err })
	config.BatchSize = 1
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(testEvent("unauthorized")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 || e.Dropped() != 1 {
		t.Fatalf("attempts=%d dropped=%d", attempts.Load(), e.Dropped())
	}
	select {
	case <-reported:
	default:
		t.Fatal("delivery failure was not reported")
	}
}

func TestQueueOverflowAndDeliveryFailureAreReported(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	errorsSeen := make(chan error, 2)
	config := testConfig(server.URL, func(err error) { errorsSeen <- err })
	config.QueueSize, config.BatchSize = 1, 1
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(testEvent("one")); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := e.Enqueue(testEvent("two")); err != nil {
		t.Fatal(err)
	}
	if err := e.Enqueue(testEvent("three")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected full queue: %v", err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if e.Dropped() != 3 || len(errorsSeen) != 2 {
		t.Fatalf("drops %d errors %d", e.Dropped(), len(errorsSeen))
	}
}

func TestInvalidEventDoesNotDiscardValidBatchNeighbors(t *testing.T) {
	var accepted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Events []Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, event := range batch.Events {
			if event.EventID == "invalid" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		for _, event := range batch.Events {
			accepted = append(accepted, event.EventID)
		}
		fmt.Fprintf(w, `{"accepted":%d}`, len(batch.Events))
	}))
	defer server.Close()
	errorsSeen := make(chan error, 1)
	config := testConfig(server.URL, func(err error) { errorsSeen <- err })
	config.BatchSize = 3
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"good1", "invalid", "good2"} {
		if err := e.Enqueue(testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if e.Dropped() != 1 || len(errorsSeen) != 1 || len(accepted) != 2 || accepted[0] != "good1" || accepted[1] != "good2" {
		t.Fatalf("accepted %v, drops %d, errors %d", accepted, e.Dropped(), len(errorsSeen))
	}
}

func TestSchemaConflictIsNotRetriedOrSplit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	errorsSeen := make(chan error, 1)
	e, err := New(testConfig(server.URL, func(err error) { errorsSeen <- err }))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err := e.Enqueue(testEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || e.Dropped() != 2 || len(errorsSeen) != 1 {
		t.Fatalf("requests %d drops %d errors %d", requests.Load(), e.Dropped(), len(errorsSeen))
	}
}

func TestConfigurationRejectsUnsafeEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://example.org", "https://user:pass@example.org", "https://example.org/path", "http://localhost:9000"} {
		if _, err := New(testConfig(endpoint, func(error) {})); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", endpoint)
		}
	}
}

func TestCircuitBreakerDropsWhileOpenAndRecovers(t *testing.T) {
	var requests atomic.Int32
	var available atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if !available.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		fmt.Fprint(w, `{"accepted":1}`)
	}))
	defer server.Close()
	errorsSeen := make(chan error, 4)
	config := testConfig(server.URL, func(err error) { errorsSeen <- err })
	config.MaxAttempts, config.CircuitFailures, config.CircuitCooldown, config.BatchSize = 1, 2, time.Hour, 1
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	nextError := func() error {
		t.Helper()
		select {
		case err := <-errorsSeen:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for exporter")
			return nil
		}
	}
	if err := e.Enqueue(testEvent("one")); err != nil {
		t.Fatal(err)
	}
	nextError()
	if !e.CircuitOpenUntil().IsZero() {
		t.Fatal("circuit opened before the threshold")
	}
	if err := e.Enqueue(testEvent("two")); err != nil {
		t.Fatal(err)
	}
	nextError()
	if e.CircuitOpenUntil().IsZero() {
		t.Fatal("circuit did not open")
	}
	if err := e.Enqueue(testEvent("three")); err != nil {
		t.Fatal(err)
	}
	if err := nextError(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open circuit was not reported: %v", err)
	}
	if requests.Load() != 2 || e.Dropped() != 3 {
		t.Fatalf("open circuit sent requests or lost drop accounting: requests=%d drops=%d", requests.Load(), e.Dropped())
	}
	available.Store(true)
	e.circuitUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if err := e.Enqueue(testEvent("four")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 || !e.CircuitOpenUntil().IsZero() || e.Dropped() != 3 {
		t.Fatalf("circuit failed to recover: requests=%d drops=%d", requests.Load(), e.Dropped())
	}
}

func TestSamplingIsDeterministicAndDoesNotCountAsDeliveryFailure(t *testing.T) {
	var accepted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Events []Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		accepted.Add(int32(len(payload.Events)))
		fmt.Fprintf(w, `{"accepted":%d}`, len(payload.Events))
	}))
	defer server.Close()
	config := testConfig(server.URL, func(err error) { t.Error(err) })
	config.QueueSize, config.BatchSize = 10000, 1000
	config.SampleRates = map[string]float64{"debug": 0.5, "trace": 0, "error": 1}
	e, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	config.SampleRates["debug"] = 0
	for i := 0; i < 2000; i++ {
		event := testEvent(fmt.Sprintf("sample-%d", i))
		event.Level = "debug"
		if err := e.Enqueue(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, level := range []string{"trace", "error", "info"} {
		event := testEvent(level)
		event.Level = level
		if err := e.Enqueue(event); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if sampled := e.Sampled(); sampled < 900 || sampled > 1100 {
		t.Fatalf("unexpected deterministic sample count: %d", sampled)
	}
	if int(e.Sampled())+int(accepted.Load()) != 2003 || e.Dropped() != 0 {
		t.Fatalf("unexpected accounting: sampled=%d delivered=%d dropped=%d", e.Sampled(), accepted.Load(), e.Dropped())
	}
	late := testEvent("late")
	late.Level = "trace"
	if err := e.Enqueue(late); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed exporter accepted sampled event: %v", err)
	}
}

func TestRejectsInvalidSamplingPolicy(t *testing.T) {
	for _, rates := range []map[string]float64{
		{"unknown": 1}, {"info": -0.1}, {"error": 1.1}, {"debug": math.NaN()}, {"trace": math.Inf(1)},
	} {
		config := testConfig("http://127.0.0.1:8098", func(error) {})
		config.SampleRates = rates
		if _, err := New(config); err == nil {
			t.Fatalf("accepted invalid sample rates: %v", rates)
		}
	}
}
