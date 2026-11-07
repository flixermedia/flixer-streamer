package exporter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Service owns a managed slog exporter and bounded asynchronous diagnostics.
type Service struct {
	Sink    *ManagedSink
	errors  chan error
	stopped chan struct{}
	once    sync.Once
	dropped atomic.Uint64
	task    slog.Handler
}

// StartService preserves the existing handler and forwards records without
// blocking application work on logger availability. Diagnostic output runs on
// a separate goroutine and is never routed back through the exporter.
func StartService(base slog.Handler, controlURL, app string, secret []byte, diagnostics io.Writer) (slog.Handler, *Service, error) {
	copyOfSecret := append([]byte(nil), secret...)
	return StartServiceWithSecretProvider(base, controlURL, app, func() ([]byte, error) { return copyOfSecret, nil }, diagnostics)
}

// StartServiceWithSecretProvider supports a shared secret published after startup.
func StartServiceWithSecretProvider(base slog.Handler, controlURL, app string, secret func() ([]byte, error), diagnostics io.Writer) (slog.Handler, *Service, error) {
	return StartServiceWithControlSocket(base, controlURL, "", app, secret, diagnostics)
}

// StartServiceWithControlSocket prefers a protected Unix socket when configured.
func StartServiceWithControlSocket(base slog.Handler, controlURL, socketPath, app string, secret func() ([]byte, error), diagnostics io.Writer) (slog.Handler, *Service, error) {
	if diagnostics == nil {
		return nil, nil, fmt.Errorf("logger diagnostics writer is required")
	}
	service := ""
	switch app {
	case "flixer-indexer":
		service = "indexer"
	case "flixer-streamer":
		service = "streamer"
	case "flixer-updater":
		service = "updater"
	default:
		return nil, nil, fmt.Errorf("invalid logger application")
	}
	var client *ControlClient
	var err error
	if socketPath != "" {
		client, err = NewUnixControlClient(socketPath, service, secret)
	} else {
		client, err = NewControlClientWithSecretProvider(controlURL, service, secret)
	}
	if err != nil {
		return nil, nil, err
	}
	var instance [16]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return nil, nil, fmt.Errorf("generate logger instance ID: %w", err)
	}
	s := &Service{errors: make(chan error, 64), stopped: make(chan struct{})}
	report := func(err error) {
		select {
		case s.errors <- err:
		default:
			s.dropped.Add(1)
		}
	}
	sink, err := NewManaged(ManagedConfig{
		Fetch: client.Fetch, RefreshInterval: 5 * time.Second,
		Exporter: Config{
			QueueSize: 1000, BatchSize: 100, FlushInterval: 500 * time.Millisecond,
			Timeout: 5 * time.Second, RetryDelay: 250 * time.Millisecond, MaxAttempts: 3, OnError: report,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	s.Sink = sink
	options := HandlerOptions{
		App: app, InstanceID: hex.EncodeToString(instance[:]), Source: app, OnError: report,
	}
	handler, err := NewSlogHandler(base, sink, options)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if closeErr := sink.Close(ctx); closeErr != nil {
			return nil, nil, fmt.Errorf("initialize logger handler: %w; close exporter: %v", err, closeErr)
		}
		return nil, nil, err
	}
	s.task, err = NewSlogHandler(slog.NewJSONHandler(io.Discard, nil), sink, options)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if closeErr := sink.Close(ctx); closeErr != nil {
			return nil, nil, fmt.Errorf("initialize task logger handler: %w; close exporter: %v", err, closeErr)
		}
		return nil, nil, err
	}
	go s.reportErrors(diagnostics)
	return handler, s, nil
}

// TaskHandler exports task records filtered by the normal handler without
// changing stdout or Sentry filtering. Call it only for otherwise omitted records.
func (s *Service) TaskHandler() slog.Handler { return s.task }

func (s *Service) reportErrors(output io.Writer) {
	var last time.Time
	var suppressed uint64
	for {
		select {
		case err := <-s.errors:
			if time.Since(last) >= time.Minute {
				fmt.Fprintf(output, "Flixer Logger export: %v (suppressed diagnostics: %d)\n", err, suppressed+s.dropped.Swap(0))
				last, suppressed = time.Now(), 0
			} else {
				suppressed++
			}
		case <-s.stopped:
			return
		}
	}
}

// Close drains best-effort delivery without exceeding the caller's deadline.
func (s *Service) Close(ctx context.Context) error {
	err := s.Sink.Close(ctx)
	s.once.Do(func() { close(s.stopped) })
	return err
}
