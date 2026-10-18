// Package transcode decides how a media file is delivered to a browser and
// runs ffmpeg to remux or re-encode it into a progressively streamed container.
package transcode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Mode string

const (
	ModeDirectPlay Mode = "DirectPlay"
	ModeRemux      Mode = "DirectStream"
	ModeTranscode  Mode = "Transcode"
)

// AllowedHeights are the output heights a client may request.
var AllowedHeights = []int{240, 360, 480, 720, 1080}

// slotWait lets a replaced request (resolution change, seek) release its slot before we reject.
const slotWait = 10 * time.Second

var (
	ErrBusy         = errors.New("too many concurrent transcodes")
	ErrInvalidArgs = errors.New("invalid transcode options")
)

type Probe struct {
	VideoCodec string  `json:"videoCodec"`
	AudioCodec string  `json:"audioCodec,omitempty"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Duration   float64 `json:"durationSeconds"`
}

type Options struct {
	// Height is the maximum output height; 0 keeps the source resolution.
	Height int
	// StartSeconds is the offset to begin streaming from.
	StartSeconds float64
}

type Plan struct {
	Mode        Mode
	ContentType string
	Args        []string
}

func ValidHeight(height int) bool {
	if height == 0 {
		return true
	}
	for _, allowed := range AllowedHeights {
		if height == allowed {
			return true
		}
	}
	return false
}

// DirectPlayable reports whether the container can be served as-is to browsers.
func DirectPlayable(fileName string) bool {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".mp4", ".m4v", ".webm":
		return true
	}
	return false
}

// Decide chooses the delivery mode for a file and the requested options.
func Decide(fileName string, probe Probe, options Options) Mode {
	if needsScaling(probe, options) {
		return ModeTranscode
	}
	if !copyableVideo(probe.VideoCodec) {
		return ModeTranscode
	}
	if DirectPlayable(fileName) && options.StartSeconds == 0 {
		return ModeDirectPlay
	}
	return ModeRemux
}

func needsScaling(probe Probe, options Options) bool {
	return options.Height > 0 && (probe.Height == 0 || options.Height < probe.Height)
}

func copyableVideo(codec string) bool {
	switch codec {
	case "h264", "vp8", "vp9", "av1":
		return true
	}
	return false
}

// BuildPlan returns the ffmpeg arguments for a remux or transcode. The output
// is fragmented MP4 (H.264/AAC) or WebM (VP8/VP9/AV1 + Opus) written to stdout.
func BuildPlan(inputPath string, probe Probe, mode Mode, options Options) (Plan, error) {
	if mode == ModeDirectPlay {
		return Plan{}, fmt.Errorf("%w: direct play does not use ffmpeg", ErrInvalidArgs)
	}
	if !ValidHeight(options.Height) || options.StartSeconds < 0 {
		return Plan{}, ErrInvalidArgs
	}

	args := []string{"-hide_banner", "-nostdin"}
	if options.StartSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(options.StartSeconds, 'f', 3, 64))
	}
	args = append(args, "-i", inputPath, "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn")

	webm := mode == ModeRemux && probe.VideoCodec != "h264"
	if mode == ModeRemux {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
			"-profile:v", "high", "-pix_fmt", "yuv420p",
		)
		if needsScaling(probe, options) {
			args = append(args, "-vf", fmt.Sprintf("scale=-2:%d", options.Height))
		}
	}

	if webm {
		if probe.AudioCodec == "opus" || probe.AudioCodec == "vorbis" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "libopus", "-b:a", "128k", "-ac", "2")
		}
		args = append(args, "-f", "webm", "pipe:1")
		return Plan{Mode: mode, ContentType: "video/webm", Args: args}, nil
	}

	if probe.AudioCodec == "aac" && mode == ModeRemux {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2")
	}
	args = append(args,
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return Plan{Mode: mode, ContentType: "video/mp4", Args: args}, nil
}

type Transcoder struct {
	ffmpegPath  string
	ffprobePath string
	slots       chan struct{}
	logger      *slog.Logger
	logLevel    string
}

// ValidLogLevel reports whether level is an ffmpeg -loglevel name.
func ValidLogLevel(level string) bool {
	switch level {
	case "quiet", "panic", "fatal", "error", "warning", "info", "verbose", "debug", "trace":
		return true
	}
	return false
}

// New creates a Transcoder. ffmpeg output at logLevel or above is forwarded to logger.
func New(ffmpegPath, ffprobePath string, maxConcurrent int, logger *slog.Logger, logLevel string) (*Transcoder, error) {
	if _, err := exec.LookPath(ffmpegPath); err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", err)
	}
	if _, err := exec.LookPath(ffprobePath); err != nil {
		return nil, fmt.Errorf("ffprobe not found: %w", err)
	}
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !ValidLogLevel(logLevel) {
		logLevel = "warning"
	}
	return &Transcoder{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
		slots:       make(chan struct{}, maxConcurrent),
		logger:      logger,
		logLevel:    logLevel,
	}, nil
}

type probeOutput struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (t *Transcoder) Probe(ctx context.Context, path string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, t.ffprobePath,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return Probe{}, fmt.Errorf("ffprobe: %w", err)
	}

	var parsed probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return Probe{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	var probe Probe
	for _, stream := range parsed.Streams {
		switch {
		case stream.CodecType == "video" && probe.VideoCodec == "":
			probe.VideoCodec, probe.Width, probe.Height = stream.CodecName, stream.Width, stream.Height
		case stream.CodecType == "audio" && probe.AudioCodec == "":
			probe.AudioCodec = stream.CodecName
		}
	}
	if probe.VideoCodec == "" {
		return Probe{}, errors.New("media has no video stream")
	}
	probe.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	return probe, nil
}

// Stream runs ffmpeg and copies its output to w. Headers are written only after
// ffmpeg produces its first bytes so a failed start can still return an error.
// Cancelling ctx (client disconnect) kills ffmpeg.
func (t *Transcoder) Stream(ctx context.Context, plan Plan, w http.ResponseWriter, head bool) error {
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(slotWait):
		return ErrBusy
	}

	// The "level" flag prefixes each line with [level] so it can be mapped to a log level.
	args := append([]string{"-loglevel", "level+" + t.logLevel}, plan.Args...)
	cmd := exec.CommandContext(ctx, t.ffmpegPath, args...)
	stderr := &limitedBuffer{limit: 4096}
	logWriter := &lineLogger{logger: t.logger.With("component", "ffmpeg", "mode", string(plan.Mode))}
	cmd.Stderr = io.MultiWriter(stderr, logWriter)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	t.logger.Info("ffmpeg started", "pid", cmd.Process.Pid, "mode", string(plan.Mode), "args", strings.Join(args, " "))
	var waitOnce sync.Once
	var waitErr error
	wait := func() error {
		waitOnce.Do(func() {
			waitErr = cmd.Wait()
			logWriter.Flush()
			t.logger.Info("ffmpeg exited", "pid", cmd.Process.Pid, "duration", time.Since(started).String(), "error", waitErr)
		})
		return waitErr
	}
	defer func() { _ = wait() }()

	reader := bufio.NewReaderSize(stdout, 64*1024)
	if _, err := reader.Peek(1); err != nil {
		_ = cmd.Process.Kill()
		_ = wait()
		return fmt.Errorf("ffmpeg produced no output: %s", strings.TrimSpace(stderr.String()))
	}

	w.Header().Set("Content-Type", plan.ContentType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Flixer-Playback-Mode", string(plan.Mode))
	w.Header().Set("Access-Control-Expose-Headers", "X-Flixer-Playback-Mode")
	w.WriteHeader(http.StatusOK)
	if head {
		_ = cmd.Process.Kill()
		return nil
	}

	controller := http.NewResponseController(w)
	buffer := make([]byte, 64*1024)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				_ = cmd.Process.Kill()
				return writeErr
			}
			_ = controller.Flush()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = cmd.Process.Kill()
			return readErr
		}
	}
	if err := wait(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// lineLogger turns ffmpeg's stderr lines into structured log records.
type lineLogger struct {
	logger *slog.Logger
	mu     sync.Mutex
	buf    []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexAny(l.buf, "\r\n")
		if i < 0 {
			break
		}
		l.emit(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *lineLogger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit(string(l.buf))
	l.buf = nil
}

func (l *lineLogger) emit(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	level := slog.LevelInfo
	if strings.HasPrefix(line, "[") {
		if end := strings.Index(line, "]"); end > 0 {
			switch line[1:end] {
			case "warning":
				level = slog.LevelWarn
			case "error", "fatal", "panic":
				level = slog.LevelError
			case "verbose", "debug", "trace":
				level = slog.LevelDebug
			}
			line = strings.TrimSpace(line[end+1:])
		}
	}
	l.logger.Log(context.Background(), level, line)
}

type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
