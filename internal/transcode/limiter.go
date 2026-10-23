package transcode

import (
	"context"
	"sync"
	"time"
)

// limiter bounds concurrent sessions and lets the bound change while sessions run.
type limiter struct {
	mu      sync.Mutex
	max     int
	inUse   int
	changed chan struct{}
}

func (l *limiter) limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.max
}

func (l *limiter) setLimit(n int) {
	if n < 0 {
		n = 0
	}
	l.mu.Lock()
	l.max = n
	l.wake()
	l.mu.Unlock()
}

// wake releases everyone waiting in acquire so they re-check. Callers hold mu.
func (l *limiter) wake() {
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

// acquire takes a slot, waiting up to wait for one. A limit of 0 means unlimited.
func (l *limiter) acquire(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		l.mu.Lock()
		if l.max == 0 || l.inUse < l.max {
			l.inUse++
			l.mu.Unlock()
			return nil
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		changed := l.changed
		l.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrBusy
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	l.inUse--
	l.wake()
	l.mu.Unlock()
}
