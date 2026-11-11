package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flixermedia/flixer-logger/pkg/exporter"
	"github.com/flixermedia/flixer-streamer/internal/config"
	"github.com/flixermedia/flixer-streamer/internal/flixer"
	"github.com/flixermedia/flixer-streamer/internal/httpserver"
	"github.com/flixermedia/flixer-streamer/internal/logging"
	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/monitoring"
	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       cfg.LogLevel,
		ReplaceAttr: logging.UnifiedAttrs,
	})).With("app", "flixer-streamer")
	exportHandler, logging, logErr := exporter.StartServiceWithControlSocket(logger.Handler(), cfg.FlixerURL, os.Getenv("FLIXER_LOGGER_CONTROL_SOCKET"), "flixer-streamer",
		func() ([]byte, error) { return []byte(cfg.TokenSecret), nil }, os.Stderr)
	if logErr != nil {
		logger.Warn("Flixer Logger export unavailable", "error", logErr)
	} else {
		logger = slog.New(exportHandler)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := logging.Close(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "Flixer Logger export shutdown:", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// flixer decides whether Sentry runs; wait up to a minute for it, then start without.
	sentryConfig := monitoring.Fetch(ctx, monitoring.FetchOptions{BaseURL: cfg.FlixerURL, App: "streamer", Logger: logger})
	sentryEnabled, flushSentry, err := monitoring.Start(sentryConfig, "")
	if err != nil {
		logger.Warn("Sentry disabled", "error", err)
	}
	if sentryEnabled {
		logger = monitoring.WrapLogger(logger)
		logger.Info("Sentry monitoring enabled")
	}
	defer flushSentry()

	// Root media folders belong to flixer; they load in the background so the streamer can start first.
	resolver := media.NewEmptyResolver()
	monitoring.Go(func() {
		flixer.SyncRoots(ctx, resolver, cfg.FlixerURL, []byte(cfg.TokenSecret), logger)
	})

	transcoder, err := transcode.New(cfg.FFmpegPath, cfg.FFprobePath, cfg.MaxTranscodes, logger, cfg.FFmpegLogLevel)
	if err != nil {
		logger.Warn("remuxing and transcoding disabled; only direct play is available", "error", err)
		transcoder = nil
	}

	if transcoder != nil {
		monitoring.Go(func() {
			flixer.SyncTranscoding(ctx, transcoder, cfg.FlixerURL, []byte(cfg.TokenSecret), logger)
		})
	}
	pluginHooks := flixer.NewPluginHooks(cfg.FlixerURL, []byte(cfg.TokenSecret), logger)
	monitoring.Go(func() { pluginHooks.Sync(ctx) })

	handler := httpserver.NewHandler(httpserver.Options{
		Secret:         []byte(cfg.TokenSecret),
		Resolver:       resolver,
		TranscodeDir:   cfg.TranscodeDir,
		TrickplayDir:   cfg.TrickplayDir,
		SubtitleDir:    cfg.SubtitleDir,
		LyricsDir:      cfg.LyricsDir,
		Transcoder:     transcoder,
		PluginHooks:    pluginHooks,
		AllowedOrigins: cfg.AllowedOrigins,
		Logger:         logger,
	})
	var rootHandler http.Handler = handler
	if sentryEnabled {
		rootHandler = monitoring.Middleware(handler)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddress(),
		Handler:           rootHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	monitoring.Go(func() {
		logger.Info("streamer listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	})

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			if closeErr := server.Close(); closeErr != nil {
				logger.Error("force close failed", "error", closeErr)
			}
			os.Exit(1)
		}
		if err := <-serverErrors; !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			os.Exit(1)
		}
		logger.Info("streamer stopped")
	}
}
