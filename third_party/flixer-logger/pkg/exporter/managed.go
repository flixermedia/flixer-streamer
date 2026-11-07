package exporter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Credential is an application-bound logger credential returned by Flixer.
type Credential struct {
	Endpoint  string
	Token     string
	ExpiresAt time.Time
}

// ManagedConfig configures a non-blocking, periodically refreshed exporter.
// Fetch and OnError must not use the logger being managed.
type ManagedConfig struct {
	Fetch           func(context.Context) (Credential, error)
	RefreshInterval time.Duration
	Exporter        Config
}

// ManagedSink suspends delivery if the control plane cannot confirm the
// configuration. A changed credential starts a new exporter so queued events
// are never sent using a credential belonging to another deployment.
type ManagedSink struct {
	mu      sync.RWMutex
	current *Exporter
	config  ManagedConfig
	active  Credential
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
}

var ErrConfigurationUnavailable = errors.New("logger producer configuration unavailable")

func NewManaged(config ManagedConfig) (*ManagedSink, error) {
	if config.Fetch == nil || config.RefreshInterval < time.Second || config.Exporter.OnError == nil {
		return nil, errors.New("invalid managed exporter configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := &ManagedSink{config: config, cancel: cancel, done: make(chan struct{})}
	go sink.run(ctx)
	return sink, nil
}

// Enqueue does no network or disk I/O and drops events while unconfigured.
func (s *ManagedSink) Enqueue(event Event) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	if s.current == nil || !s.active.ExpiresAt.After(time.Now()) {
		return ErrConfigurationUnavailable
	}
	return s.current.Enqueue(event)
}

func (s *ManagedSink) run(ctx context.Context) {
	defer close(s.done)
	s.refresh(ctx)
	ticker := time.NewTicker(s.config.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.replace(nil, Credential{}, false)
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

func (s *ManagedSink) refresh(ctx context.Context) {
	credential, err := s.config.Fetch(ctx)
	if err == nil && !credential.ExpiresAt.After(time.Now()) {
		err = errors.New("logger producer credential has expired")
	}
	if err != nil {
		s.replace(nil, Credential{}, true)
		if ctx.Err() == nil {
			s.config.Exporter.OnError(err)
		}
		return
	}
	s.mu.RLock()
	unchanged := s.current != nil && s.active == credential
	s.mu.RUnlock()
	if unchanged {
		return
	}
	config := s.config.Exporter
	config.Endpoint, config.Token = credential.Endpoint, credential.Token
	next, err := New(config)
	if err != nil {
		s.replace(nil, Credential{}, true)
		s.config.Exporter.OnError(err)
		return
	}
	s.replace(next, credential, true)
}

func (s *ManagedSink) replace(next *Exporter, credential Credential, discard bool) {
	s.mu.Lock()
	if s.closed && next != nil {
		s.mu.Unlock()
		s.drain(next)
		return
	}
	previous := s.current
	s.current, s.active = next, credential
	var droppedBefore uint64
	if previous != nil && discard {
		droppedBefore = previous.Dropped()
		previous.Abort()
	}
	s.mu.Unlock()
	if previous != nil {
		s.drain(previous)
		if dropped := previous.Dropped() - droppedBefore; discard && dropped > 0 {
			s.config.Exporter.OnError(fmt.Errorf("logger configuration changed or became unavailable; %d events discarded", dropped))
		}
	}
}

func (s *ManagedSink) drain(previous *Exporter) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := previous.Close(ctx); err != nil {
		s.config.Exporter.OnError(err)
	}
}

// Close stops acceptance and waits for the manager until the supplied deadline.
func (s *ManagedSink) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
