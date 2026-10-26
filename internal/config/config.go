package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

type Config struct {
	Port           string
	TokenSecret    string
	FFmpegPath     string
	FFmpegLogLevel string
	FFprobePath    string
	MaxTranscodes  int
	TranscodeDir   string
	TrickplayDir   string
	SubtitleDir    string
	LyricsDir      string
	AllowedOrigins []string
	LogLevel       slog.Level
	LogDir         string
	FlixerURL      string
}

// secretFileWait is how long to wait for flixer to publish the shared secret on first start.
const secretFileWait = 2 * time.Minute

// ReadSecretFile returns the trimmed contents of path, waiting up to wait for it to appear. An empty path gives "".
func ReadSecretFile(path string, wait time.Duration) (string, error) {
	if path == "" {
		return "", nil
	}
	deadline := time.Now().Add(wait)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data)), nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read APP_SECRET_FILE: %w", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("APP_SECRET_FILE %s was not published by flixer in time", path)
		}
		time.Sleep(time.Second)
	}
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

	secret := strings.TrimSpace(legacyEnv("APP_SECRET", "STREAM_TOKEN_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("JWT_SECRET"))
	}
	if secret == "" {
		fromFile, err := ReadSecretFile(legacyEnv("APP_SECRET_FILE", "STREAM_TOKEN_SECRET_FILE"), secretFileWait)
		if err != nil {
			return Config{}, err
		}
		secret = fromFile
	}
	if len([]byte(secret)) < 32 {
		return Config{}, errors.New("APP_SECRET, APP_SECRET_FILE or JWT_SECRET must contain at least 32 bytes")
	}

	level, err := parseLogLevel(envOrDefault("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}

	maxTranscodes, err := strconv.Atoi(envOrDefault("MAX_TRANSCODES", "0"))
	if err != nil || maxTranscodes < 0 {
		return Config{}, errors.New("MAX_TRANSCODES must be a non-negative integer (0 means unlimited)")
	}
	ffmpegPath := envOrDefault("FFMPEG_PATH", "ffmpeg")
	ffmpegLogLevel := strings.ToLower(envOrDefault("FFMPEG_LOG_LEVEL", "info"))
	if !transcode.ValidLogLevel(ffmpegLogLevel) {
		return Config{}, errors.New("FFMPEG_LOG_LEVEL must be one of quiet, panic, fatal, error, warning, info, verbose, debug, trace")
	}

	return Config{
		Port:           port,
		TokenSecret:    secret,
		FFmpegPath:     ffmpegPath,
		FFmpegLogLevel: ffmpegLogLevel,
		FFprobePath:    envOrDefault("FFPROBE_PATH", siblingProbePath(ffmpegPath)),
		MaxTranscodes:  maxTranscodes,
		TranscodeDir:   envOrDefault("TRANSCODE_DIR", dataSubdir("cache/transcode", "./cache/transcode")),
		TrickplayDir:   envOrDefault("TRICKPLAY_DIR", dataSubdir("cache/trickplay", "/data/cache/trickplay")),
		LyricsDir:      envOrDefault("LYRICS_PATH", dataSubdir("lyrics", "./data/lyrics")),
		SubtitleDir:    envOrDefault("SUBTITLE_DIR", filepath.Join(filepath.Dir(envOrDefault("TRICKPLAY_DIR", dataSubdir("cache/trickplay", "/data/cache/trickplay"))), "subtitles")),
		AllowedOrigins: splitNonEmpty(os.Getenv("ALLOWED_ORIGINS"), ","),
		LogLevel:       level,
		LogDir:         logDirectory(),
		FlixerURL:      strings.TrimRight(envOrDefault("FLIXER_URL", "http://localhost:8083"), "/"),
	}, nil
}

func logDirectory() string {
	if dir := strings.TrimSpace(os.Getenv("LOG_DIR")); dir != "" {
		return dir
	}
	if dir := dataRoot(); dir != "" {
		return filepath.Join(dir, "logs")
	}
	return ""
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

// legacyEnv reads name, falling back to the variable it replaced so existing deployments keep working.
func legacyEnv(name, legacy string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return os.Getenv(legacy)
}

// dataRoot is DATA_PATH, inheriting DATA_DIR in development where there is no container mount.
func dataRoot() string {
	return strings.TrimSpace(envOrDefault("DATA_PATH", os.Getenv("DATA_DIR")))
}

func dataSubdir(sub, fallback string) string {
	if root := dataRoot(); root != "" {
		return filepath.Join(root, sub)
	}
	return fallback
}
