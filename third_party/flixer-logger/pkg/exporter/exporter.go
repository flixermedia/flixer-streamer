// Package exporter asynchronously ships structured batches to Flixer Logger.
package exporter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event uses the logger's versioned ingestion schema. The caller must assign
// a stable EventID and remove sensitive information before enqueueing.
type Event struct {
	SchemaVersion int            `json:"schema_version"`
	EventID       string         `json:"event_id"`
	Time          time.Time      `json:"time"`
	Level         string         `json:"level"`
	InstanceID    string         `json:"instance_id"`
	HostID        string         `json:"host_id,omitempty"`
	App           string         `json:"app"`
	Source        string         `json:"source,omitempty"`
	Code          string         `json:"event,omitempty"`
	Message       string         `json:"message"`
	TraceID       string         `json:"trace_id,omitempty"`
	SpanID        string         `json:"span_id,omitempty"`
	RequestID     string         `json:"request_id,omitempty"`
	TaskID        string         `json:"task_id,omitempty"`
	DurationMS    *float64       `json:"duration_ms,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
	Error         *ErrorDetails  `json:"error,omitempty"`
}

type ErrorDetails struct {
	Type  string `json:"type,omitempty"`
	Code  string `json:"code,omitempty"`
	Stack string `json:"stack,omitempty"`
}

type Config struct {
	Endpoint      string
	Token         string
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	Timeout       time.Duration
	RetryDelay    time.Duration
	MaxAttempts   int
	// Zero selects the default of three failed batches and a 30-second cooldown.
	CircuitFailures int
	CircuitCooldown time.Duration
	// SampleRates sets the fraction retained for each level; omitted levels retain all events.
	SampleRates map[string]float64
	// OnError must not call back into the exporter or log via the exporter.
	OnError func(error)
}

// Exporter is safe for concurrent calls to Enqueue and Close.
type Exporter struct {
	config              Config
	client              *http.Client
	queue               chan []byte
	mu                  sync.RWMutex
	token               string
	closed              bool
	done                chan struct{}
	ctx                 context.Context
	cancel              context.CancelFunc
	drops               atomic.Uint64
	sampled             atomic.Uint64
	circuitUntil        atomic.Int64
	consecutiveFailures int
}

// New starts one worker with a bounded queue and finite retry policy.
func New(config Config) (*Exporter, error) {
	if config.CircuitFailures == 0 {
		config.CircuitFailures = 3
	}
	if config.CircuitCooldown == 0 {
		config.CircuitCooldown = 30 * time.Second
	}
	rates := make(map[string]float64, len(config.SampleRates))
	for level, rate := range config.SampleRates {
		switch level {
		case "trace", "debug", "info", "warn", "error", "fatal":
		default:
			return nil, errors.New("invalid sampling level")
		}
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
			return nil, errors.New("sampling rate must be between zero and one")
		}
		rates[level] = rate
	}
	config.SampleRates = rates
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return nil, errors.New("logger endpoint must be an origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("logger endpoint requires HTTPS except on loopback")
		}
	}
	if !validToken(config.Token) {
		return nil, errors.New("invalid logger token")
	}
	if config.QueueSize < 1 || config.QueueSize > 100000 || config.BatchSize < 1 || config.BatchSize > 1000 ||
		config.FlushInterval <= 0 || config.Timeout <= 0 || config.RetryDelay <= 0 ||
		config.MaxAttempts < 1 || config.MaxAttempts > 20 || config.CircuitFailures < 1 || config.CircuitFailures > 100 ||
		config.CircuitCooldown < time.Millisecond || config.CircuitCooldown > time.Hour || config.OnError == nil {
		return nil, errors.New("invalid exporter configuration")
	}
	u.Path = "/api/logs/batch"
	config.Endpoint = u.String()
	token := config.Token
	config.Token = ""
	client, err := newLoggerHTTPClient(config.Timeout)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Exporter{
		config: config, client: client,
		token: token, queue: make(chan []byte, config.QueueSize), done: make(chan struct{}), ctx: ctx, cancel: cancel,
	}
	go e.run()
	return e, nil
}

var ErrClosed = errors.New("exporter is closed")
var ErrQueueFull = errors.New("exporter queue is full")
var errInvalidBatch = errors.New("logger rejected invalid batch")
var errTransientDelivery = errors.New("transient logger delivery failure")
var ErrCircuitOpen = errors.New("logger exporter circuit is open")

func validToken(token string) bool {
	return len(token) >= 32 && len(token) <= 1024 && !strings.ContainsAny(token, "\r\n")
}

// RotateToken switches future attempts to a new credential without changing the
// destination. The caller must verify that the replacement belongs to the same
// deployment and application. An in-flight request may use the old credential.
func (e *Exporter) RotateToken(token string) error {
	if !validToken(token) {
		return errors.New("invalid logger token")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	e.token = token
	return nil
}

// Enqueue snapshots an event immediately and never waits on network or disk.
// Queue overflow and invalid events are returned to the caller.
func (e *Exporter) Enqueue(event Event) error {
	if event.SchemaVersion != 1 || event.EventID == "" || event.Time.IsZero() ||
		event.Level == "" || event.App == "" || event.InstanceID == "" || event.Message == "" {
		return errors.New("invalid event")
	}
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return ErrClosed
	}
	if rate, ok := e.config.SampleRates[event.Level]; ok && rate < 1 {
		digest := sha256.Sum256([]byte(event.EventID))
		value := binary.BigEndian.Uint64(digest[:8]) >> 11
		if rate == 0 || float64(value) >= rate*(1<<53) {
			e.sampled.Add(1)
			e.mu.RUnlock()
			return nil
		}
	}
	e.mu.RUnlock()
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("serialize log event: %w", err)
	}
	if len(payload) > 64*1024 {
		return errors.New("event exceeds size limit")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return ErrClosed
	}
	select {
	case e.queue <- payload:
		return nil
	default:
		e.drops.Add(1)
		return ErrQueueFull
	}
}

// Dropped returns events dropped due to overflow or exhausted delivery retries.
func (e *Exporter) Dropped() uint64 { return e.drops.Load() }

// Sampled returns events intentionally omitted by the configured policy.
func (e *Exporter) Sampled() uint64 { return e.sampled.Load() }

// CircuitOpenUntil returns the end of the current cooldown, or zero when closed.
func (e *Exporter) CircuitOpenUntil() time.Time {
	until := e.circuitUntil.Load()
	if until == 0 || time.Now().UnixNano() >= until {
		return time.Time{}
	}
	return time.Unix(0, until)
}

// Close stops acceptance and flushes queued events subject to retry and timeout
// limits. A caller deadline does not cancel the independent worker.
func (e *Exporter) Close(ctx context.Context) error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	e.mu.Unlock()
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Abort stops delivery and discards queued events. An already accepted request
// cannot be recalled, even when cancellation reaches the client in time.
func (e *Exporter) Abort() {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	e.cancel()
	e.mu.Unlock()
}

func (e *Exporter) run() {
	defer func() {
		e.cancel()
		close(e.done)
	}()
	ticker := time.NewTicker(e.config.FlushInterval)
	defer ticker.Stop()
	batch := make([][]byte, 0, e.config.BatchSize)
	batchBytes := len(`{"events":[]}`)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.deliver(batch)
		batch = batch[:0]
		batchBytes = len(`{"events":[]}`)
	}
	for {
		if e.ctx.Err() != nil {
			e.drops.Add(uint64(len(batch) + len(e.queue)))
			return
		}
		select {
		case <-e.ctx.Done():
			e.drops.Add(uint64(len(batch) + len(e.queue)))
			return
		case event, ok := <-e.queue:
			if !ok {
				if e.ctx.Err() != nil {
					e.drops.Add(uint64(len(batch)))
					return
				}
				flush()
				return
			}
			if batchBytes+len(event)+1 > 2*1024*1024 {
				flush()
			}
			batch = append(batch, event)
			batchBytes += len(event) + 1
			if len(batch) == e.config.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Isolate invalid records so one rejected event cannot discard valid neighbors.
func (e *Exporter) deliver(batch [][]byte) {
	if e.ctx.Err() != nil {
		e.drops.Add(uint64(len(batch)))
		return
	}
	if !e.CircuitOpenUntil().IsZero() {
		e.drops.Add(uint64(len(batch)))
		e.config.OnError(ErrCircuitOpen)
		return
	}
	err := e.send(batch)
	if err == nil {
		e.consecutiveFailures = 0
		e.circuitUntil.Store(0)
		return
	}
	if errors.Is(err, errInvalidBatch) && len(batch) > 1 {
		middle := len(batch) / 2
		e.deliver(batch[:middle])
		e.deliver(batch[middle:])
		return
	}
	if errors.Is(err, errTransientDelivery) {
		e.consecutiveFailures++
		if e.consecutiveFailures >= e.config.CircuitFailures {
			e.circuitUntil.Store(time.Now().Add(e.config.CircuitCooldown).UnixNano())
			e.consecutiveFailures = 0
		}
	} else {
		e.consecutiveFailures = 0
	}
	e.drops.Add(uint64(len(batch)))
	e.config.OnError(err)
}

func (e *Exporter) send(batch [][]byte) error {
	var body bytes.Buffer
	body.WriteString(`{"events":[`)
	for i, event := range batch {
		if i != 0 {
			body.WriteByte(',')
		}
		body.Write(event)
	}
	body.WriteString("]}")
	var lastErr error
	for attempt := 0; attempt < e.config.MaxAttempts; attempt++ {
		var retryAfter time.Duration
		req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.config.Endpoint, bytes.NewReader(body.Bytes()))
		if err != nil {
			return fmt.Errorf("create batch request: %w", err)
		}
		e.mu.RLock()
		token := e.token
		e.mu.RUnlock()
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Flixer-Log-Schema", "1")
		resp, err := e.client.Do(req)
		if err == nil {
			responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
			closeErr := resp.Body.Close()
			switch {
			case readErr != nil:
				err = fmt.Errorf("read batch response: %w", readErr)
			case closeErr != nil:
				err = fmt.Errorf("close batch response: %w", closeErr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				var ack struct {
					Accepted int `json:"accepted"`
				}
				if len(responseBody) > 4096 || json.Unmarshal(responseBody, &ack) != nil || ack.Accepted != len(batch) {
					err = errors.New("invalid logger batch acknowledgement")
				} else {
					return nil
				}
			case resp.StatusCode == http.StatusBadRequest:
				return fmt.Errorf("%w: HTTP %d", errInvalidBatch, resp.StatusCode)
			case resp.StatusCode == http.StatusUnauthorized && attempt+1 < e.config.MaxAttempts:
				e.mu.RLock()
				rotated := e.token != token
				e.mu.RUnlock()
				if rotated {
					continue
				}
				return fmt.Errorf("logger rejected batch: HTTP %d", resp.StatusCode)
			case resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500:
				return fmt.Errorf("logger rejected batch: HTTP %d", resp.StatusCode)
			default:
				err = fmt.Errorf("logger unavailable: HTTP %d", resp.StatusCode)
				retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			}
		}
		lastErr = err
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		if attempt+1 < e.config.MaxAttempts {
			delay := retryBackoff(e.config.RetryDelay, attempt)
			if retryAfter > delay {
				delay = retryAfter
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-e.ctx.Done():
				timer.Stop()
				return e.ctx.Err()
			}
		}
	}

	return fmt.Errorf("%w after %d attempts: %w", errTransientDelivery, e.config.MaxAttempts, lastErr)
}
