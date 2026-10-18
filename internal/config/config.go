package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

type MediaRoot struct {
	ID   string
	Path string
}

type Config struct {
	Port           string
	TokenSecret    string
	MediaRoots     []MediaRoot
	FFmpegPath     string
	FFmpegLogLevel string
	FFprobePath    string
	MaxTranscodes  int
	TranscodeDir   string
	AllowedOrigins []string
	LogLevel       slog.Level
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}

	port := strings.TrimSpace(envOrDefault("PORT", "8091"))
	port = strings.TrimPrefix(port, ":")
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return Config{}, errors.New("PORT must be a valid port number from 1 to 65535")
	}

	secret := strings.TrimSpace(os.Getenv("STREAM_TOKEN_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("JWT_SECRET"))
	}
	if len([]byte(secret)) < 32 {
		return Config{}, errors.New("STREAM_TOKEN_SECRET or JWT_SECRET must contain at least 32 bytes")
	}

	roots, err := parseMediaRoots(os.Getenv("MEDIA_ROOTS"))
	if err != nil {
		return Config{}, err
	}

	level, err := parseLogLevel(envOrDefault("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}

	maxTranscodes, err := strconv.Atoi(envOrDefault("MAX_TRANSCODES", "2"))
	if err != nil || maxTranscodes < 1 {
		return Config{}, errors.New("MAX_TRANSCODES must be a positive integer")
	}
	ffmpegPath := envOrDefault("FFMPEG_PATH", "ffmpeg")
	ffmpegLogLevel := strings.ToLower(envOrDefault("FFMPEG_LOG_LEVEL", "info"))
	if !transcode.ValidLogLevel(ffmpegLogLevel) {
		return Config{}, errors.New("FFMPEG_LOG_LEVEL must be one of quiet, panic, fatal, error, warning, info, verbose, debug, trace")
	}

	return Config{
		Port:           port,
		TokenSecret:    secret,
		MediaRoots:     roots,
		FFmpegPath:     ffmpegPath,
		FFmpegLogLevel: ffmpegLogLevel,
		FFprobePath:    envOrDefault("FFPROBE_PATH", siblingProbePath(ffmpegPath)),
		MaxTranscodes:  maxTranscodes,
		TranscodeDir:   envOrDefault("TRANSCODE_DIR", "./cache/transcode"),
		AllowedOrigins: splitNonEmpty(os.Getenv("ALLOWED_ORIGINS"), ","),
		LogLevel:       level,
	}, nil
}

func (c Config) ListenAddress() string {
	return ":" + c.Port
}

// siblingProbePath derives the ffprobe location from an explicit ffmpeg path.
func siblingProbePath(ffmpegPath string) string {
	dir := filepath.Dir(ffmpegPath)
	if dir == "." {
		return "ffprobe"
	}
	return filepath.Join(dir, "ffprobe"+filepath.Ext(ffmpegPath))
}

func parseMediaRoots(value string) ([]MediaRoot, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("MEDIA_ROOTS must contain at least one directory")
	}

	var paths []string
	if strings.HasPrefix(value, "[") {
		if err := json.Unmarshal([]byte(value), &paths); err != nil {
			return nil, fmt.Errorf("parse MEDIA_ROOTS JSON array: %w", err)
		}
	} else {
		paths = filepath.SplitList(value)
	}

	roots := make([]MediaRoot, 0, len(paths))
	for i, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("MEDIA_ROOTS entry %d is empty", i)
		}
		roots = append(roots, MediaRoot{
			ID:   fmt.Sprintf("root-%d", i+1),
			Path: path,
		})
	}
	if len(roots) == 0 {
		return nil, errors.New("MEDIA_ROOTS must contain at least one directory")
	}
	return roots, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
}

func loadDotEnv(file string) error {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf(".env line %d must be KEY=VALUE", lineNumber)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf(".env line %d: %w", lineNumber, err)
			}
			value = unquoted
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf(".env line %d: set %s: %w", lineNumber, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitNonEmpty(value, separator string) []string {
	var values []string
	for _, item := range strings.Split(value, separator) {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}
