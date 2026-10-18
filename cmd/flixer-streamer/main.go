package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/config"
	"github.com/flixermedia/flixer-streamer/internal/httpserver"
	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	roots := make([]media.Root, 0, len(cfg.MediaRoots))
	for _, root := range cfg.MediaRoots {
		roots = append(roots, media.Root{ID: root.ID, Path: root.Path})
	}
	resolver, err := media.NewResolver(roots)
	if err != nil {
		logger.Error("configure media roots", "error", err)
		os.Exit(1)
	}

	transcoder, err := transcode.New(cfg.FFmpegPath, cfg.FFprobePath, cfg.MaxTranscodes, logger, cfg.FFmpegLogLevel)
	if err != nil {
		logger.Warn("remuxing and transcoding disabled; only direct play is available", "error", err)
		transcoder = nil
	}

	handler := httpserver.NewHandler(httpserver.Options{
		Secret:         []byte(cfg.TokenSecret),
		Resolver:       resolver,
		Transcoder:     transcoder,
		AllowedOrigins: cfg.AllowedOrigins,
		Logger:         logger,
	})
	server := &http.Server{
		Addr:              cfg.ListenAddress(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("streamer listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

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
