package transcode

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// idleGrace keeps an HLS session listed between segment requests.
	idleGrace = 30 * time.Second
	// revokeTTL matches the lifetime of a stream token.
	revokeTTL = time.Hour
)

// ErrCancelled is returned when an administrator cancelled the session.
var ErrCancelled = errors.New("session cancelled by an administrator")

// SessionMeta describes who is playing what; it is supplied by the HTTP layer.
type SessionMeta struct {
	ID         string `json:"id"`
	UserID     string `json:"userId,omitempty"`
	MediaItem  string `json:"mediaItemId,omitempty"`
	File       string `json:"file"`
	Delivery   string `json:"delivery"` // "progressive" or "hls"
	RemoteAddr string `json:"remoteAddr,omitempty"`
	UserAgent  string `json:"userAgent,omitempty"`
	Source     Probe  `json:"source"`
}

// SessionInfo is a point-in-time snapshot of a session for the admin API.
type SessionInfo struct {
	SessionMeta
	Mode            Mode      `json:"mode"`
	Height          int       `json:"height"`
	PositionSeconds float64   `json:"positionSeconds"`
	StartedAt       time.Time `json:"startedAt"`
	LastActivityAt  time.Time `json:"lastActivityAt"`
	BytesSent       int64     `json:"bytesSent"`
	Running         bool      `json:"running"`
	PID             int       `json:"pid,omitempty"`
}

type session struct {
	meta      SessionMeta
	mode      Mode
	height    int
	position  float64
	startedAt time.Time
	lastSeen  time.Time
	bytes     atomic.Int64
	pid       int
	runs      map[*run]struct{}
}

type run struct {
	cancel func()
	reason atomic.Bool
}

// registry tracks logical playback sessions across the ffmpeg runs that serve them.
type registry struct {
	mu       sync.Mutex
	sessions map[string]*session
	revoked  map[string]time.Time
}

func newRegistry() *registry {
	return &registry{sessions: map[string]*session{}, revoked: map[string]time.Time{}}
}

// handle is held by one ffmpeg run for the duration of its stream.
type handle struct {
	reg  *registry
	sess *session
	run  *run
}

func newID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// begin registers an ffmpeg run. It fails with ErrCancelled when the session was revoked.
func (r *registry) begin(meta SessionMeta, mode Mode, height int, position float64, cancel func()) (*handle, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	if meta.ID == "" {
		meta.ID = newID()
	}
	if _, revoked := r.revoked[meta.ID]; revoked {
		return nil, ErrCancelled
	}
	sess := r.sessions[meta.ID]
	if sess == nil {
		sess = &session{meta: meta, startedAt: now, runs: map[*run]struct{}{}}
		r.sessions[meta.ID] = sess
	}
	sess.meta = meta
	sess.mode, sess.height, sess.position, sess.lastSeen = mode, height, position, now
	current := &run{cancel: cancel}
	sess.runs[current] = struct{}{}
	return &handle{reg: r, sess: sess, run: current}, nil
}

func (h *handle) setPID(pid int) {
	h.reg.mu.Lock()
	h.sess.pid = pid
	h.reg.mu.Unlock()
}

func (h *handle) addBytes(n int) { h.sess.bytes.Add(int64(n)) }

// cancelled reports whether this run was stopped by an administrator.
func (h *handle) cancelled() bool { return h.run.reason.Load() }

func (h *handle) end() {
	h.reg.mu.Lock()
	delete(h.sess.runs, h.run)
	h.sess.lastSeen = time.Now()
	if len(h.sess.runs) == 0 {
		h.sess.pid = 0
	}
	h.reg.mu.Unlock()
}

func (r *registry) pruneLocked(now time.Time) {
	for id, sess := range r.sessions {
		if len(sess.runs) == 0 && now.Sub(sess.lastSeen) > idleGrace {
			delete(r.sessions, id)
		}
	}
	for id, expires := range r.revoked {
		if now.After(expires) {
			delete(r.revoked, id)
		}
	}
}

func (r *registry) list() []SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	out := make([]SessionInfo, 0, len(r.sessions))
	for _, sess := range r.sessions {
		out = append(out, SessionInfo{
			SessionMeta:     sess.meta,
			Mode:            sess.mode,
			Height:          sess.height,
			PositionSeconds: sess.position,
			StartedAt:       sess.startedAt,
			LastActivityAt:  sess.lastSeen,
			BytesSent:       sess.bytes.Load(),
			Running:         len(sess.runs) > 0,
			PID:             sess.pid,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// cancel stops every ffmpeg run of the session and blocks it from restarting.
func (r *registry) cancel(id string) bool {
	r.mu.Lock()
	sess := r.sessions[id]
	if sess == nil {
		r.mu.Unlock()
		return false
	}
	r.revoked[id] = time.Now().Add(revokeTTL)
	runs := make([]*run, 0, len(sess.runs))
	for current := range sess.runs {
		runs = append(runs, current)
	}
	delete(r.sessions, id)
	r.mu.Unlock()

	for _, current := range runs {
		current.reason.Store(true)
		current.cancel()
	}
	return true
}
