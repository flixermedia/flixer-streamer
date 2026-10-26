package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/config"
	"github.com/flixermedia/flixer-streamer/internal/flixer"
	"github.com/flixermedia/flixer-streamer/internal/httpserver"
	"github.com/flixermedia/flixer-streamer/internal/logfile"
	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/monitoring"
	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

const (
	logFileMaxBytes = 10 * 1024 * 1024
	logFileBackups  = 3
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	var logOutput io.Writer = os.Stdout
	if cfg.LogDir != "" {
		file, err := logfile.Open(filepath.Join(cfg.LogDir, "streamer.log"), logFileMaxBytes, logFileBackups)
		if err != nil {
			slog.Error("open log file", "error", err)
			os.Exit(1)
		}
		defer file.Close()
		logOutput = io.MultiWriter(os.Stdout, file)
	}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{
		Level:       cfg.LogLevel,
		ReplaceAttr: logfile.UnifiedAttrs,
	})).With("app", "flixer-streamer")

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

	handler := httpserver.NewHandler(httpserver.Options{
		Secret:         []byte(cfg.TokenSecret),
		Resolver:       resolver,
		TrickplayDir:   cfg.TrickplayDir,
		SubtitleDir:    cfg.SubtitleDir,
		LyricsDir:      cfg.LyricsDir,
		Transcoder:     transcoder,
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
