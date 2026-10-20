// Package monitoring wires up optional Sentry error reporting. Whether Sentry is
// enabled, and which DSN it uses, is owned by flixer and fetched at startup.
package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

const (
	// DefaultFetchTimeout is how long startup waits for flixer before giving up.
	DefaultFetchTimeout = 60 * time.Second
	defaultInterval     = 2 * time.Second
	requestTimeout      = 5 * time.Second
	flushTimeout        = 2 * time.Second
)

// Config is the Sentry configuration flixer returns for an app.
type Config struct {
	Enabled bool   `json:"enabled"`
	DSN     string `json:"dsn"`
}

// FetchOptions controls how the configuration is requested from flixer.
type FetchOptions struct {
	BaseURL  string
	App      string
	Timeout  time.Duration
	Interval time.Duration
	Client   *http.Client
	Logger   *slog.Logger
}

// Fetch asks flixer for the app's Sentry configuration, retrying until Timeout
// elapses. If flixer cannot be reached it returns a disabled configuration so
// the caller can continue starting without Sentry.
func Fetch(ctx context.Context, opts FetchOptions) Config {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultFetchTimeout
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: requestTimeout}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	endpoint := strings.TrimRight(opts.BaseURL, "/") + "/api/sentry/config/" + url.PathEscape(opts.App)
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	for {
		cfg, err := request(ctx, opts.Client, endpoint)
		if err == nil {
			return cfg
		}

		select {
		case <-ctx.Done():
			opts.Logger.Warn("could not reach flixer for Sentry configuration; continuing with Sentry disabled",
				"timeout", opts.Timeout.String(), "error", err)
			return Config{}
		case <-time.After(opts.Interval):
		}
	}
}

func request(ctx context.Context, client *http.Client, endpoint string) (Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Config{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Config{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("flixer returned status %d", resp.StatusCode)
	}

	var cfg Config
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode Sentry configuration: %w", err)
	}
	cfg.DSN = strings.TrimSpace(cfg.DSN)
	return cfg, nil
}

// Start initialises Sentry when cfg enables it. The returned flush function is
// always safe to call.
func Start(cfg Config, release string) (enabled bool, flush func(), err error) {
	noop := func() {}
	if !cfg.Enabled || cfg.DSN == "" {
		return false, noop, nil
	}
	if err := sentry.Init(sentry.ClientOptions{Dsn: cfg.DSN, Release: release}); err != nil {
		return false, noop, fmt.Errorf("initialise Sentry: %w", err)
	}
	return true, func() { sentry.Flush(flushTimeout) }, nil
}

// Middleware reports panics to Sentry and re-panics so net/http still recovers.
func Middleware(next http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(next)
}

// Go runs fn in a goroutine, reporting any panic to Sentry before re-panicking so
// the crash is still visible. Use it for background goroutines outside HTTP handlers.
func Go(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				sentry.CurrentHub().Recover(r)
				sentry.Flush(flushTimeout)
				panic(r)
			}
		}()
		fn()
	}()
}

// WrapLogger returns a logger that also reports error-level records to Sentry.
func WrapLogger(logger *slog.Logger) *slog.Logger {
	return slog.New(&captureHandler{Handler: logger.Handler()})
}

type captureHandler struct {
	slog.Handler
}

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError {
		var cause error
		record.Attrs(func(attr slog.Attr) bool {
			if err, ok := attr.Value.Any().(error); ok {
				cause = err
				return false
			}
			return true
		})
		if cause == nil {
			cause = errors.New(record.Message)
		} else {
			cause = fmt.Errorf("%s: %w", record.Message, cause)
		}
		sentry.CaptureException(cause)
	}
	return h.Handler.Handle(ctx, record)
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{Handler: h.Handler.WithGroup(name)}
}
