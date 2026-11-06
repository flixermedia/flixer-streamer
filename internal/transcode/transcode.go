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
	"math"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// AllowedAudioBitrates are the MPEG-1 Layer III CBR rates supported for native audio playback.
var AllowedAudioBitrates = []int{128, 160, 192, 224, 256, 320}

// slotWait lets a replaced request (resolution change, seek) release its slot before we reject.
const slotWait = 10 * time.Second

var (
	ErrBusy        = errors.New("too many concurrent transcodes")
	ErrInvalidArgs = errors.New("invalid transcode options")
)

type Probe struct {
	VideoCodec  string       `json:"videoCodec"`
	AudioCodec  string       `json:"audioCodec,omitempty"`
	AudioTracks []AudioTrack `json:"audioTracks"`
	Width       int          `json:"width"`
	Height      int          `json:"height"`
	Duration    float64      `json:"durationSeconds"`
}

type AudioTrack struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Default  bool   `json:"default"`
}

func (p Probe) HasAudioTrack(index int) bool {
	for _, track := range p.AudioTracks {
		if track.Index == index {
			return true
		}
	}
	return false
}

type Options struct {
	// Height is the maximum output height; 0 keeps the source resolution.
	Height int
	// AudioBitrateKbps requests an MPEG-1 Layer III CBR audio stream; 0 leaves audio unchanged.
	AudioBitrateKbps int
	// StartSeconds is the offset to begin streaming from.
	StartSeconds float64
	// KeyframeStart marks StartSeconds as a video keyframe position, so a
	// stream-copy output begins exactly there for both audio and video.
	KeyframeStart bool
	// AudioStreamIndex selects an audio stream by its absolute container index.
	AudioStreamIndex *int
	// Encoder holds the admin's video encoder settings; the zero value means the defaults.
	Encoder Settings
	// Compat asks for WebM output a browser without H.264/AAC support can play:
	// "vp9" (VP9 + Opus) or "vp8" (VP8 + Vorbis). Empty keeps H.264/AAC.
	Compat string
}

// ValidCompat reports whether value is an accepted compatibility profile.
func ValidCompat(value string) bool {
	return value == "" || value == "vp9" || value == "vp8"
}

type Plan struct {
	Mode        Mode
	ContentType string
	Args        []string
	// Meta, Height and Position describe the session for the admin API.
	Meta     SessionMeta
	Height   int
	Position float64
	// Encrypt, when set, AES-128-CBC encrypts the output (HLS segments).
	Encrypt *Encryption
}

// Encryption holds the AES-128 key and IV for an encrypted HLS segment.
type Encryption struct{ Key, IV []byte }

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

func ValidAudioBitrate(bitrate int) bool {
	for _, allowed := range AllowedAudioBitrates {
		if bitrate == allowed {
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
	if options.Compat != "" {
		return ModeTranscode
	}
	if needsScaling(probe, options) {
		return ModeTranscode
	}
	if !copyableVideo(probe.VideoCodec) {
		return ModeTranscode
	}
	if DirectPlayable(fileName) && options.StartSeconds == 0 && options.AudioStreamIndex == nil {
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
	if options.AudioStreamIndex != nil && !probe.HasAudioTrack(*options.AudioStreamIndex) {
		return Plan{}, ErrInvalidArgs
	}

	args := []string{"-hide_banner", "-nostdin"}
	if mode == ModeTranscode {
		args = append(args, options.Encoder.inputArgs()...)
	}
	if options.StartSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(options.StartSeconds, 'f', 3, 64))
	}
	args = append(args, "-i", inputPath)
	if mode == ModeRemux && options.KeyframeStart {
		// Without this ffmpeg also copies everything from the demuxer's coarser
		// seek point (often ~10s earlier), shifting the timeline.
		args = append(args, "-copypriorss", "0")
	}
	audioMap := "0:a:0?"
	audioCodec := probe.AudioCodec
	if options.AudioStreamIndex != nil {
		audioMap = "0:" + strconv.Itoa(*options.AudioStreamIndex)
		for _, track := range probe.AudioTracks {
			if track.Index == *options.AudioStreamIndex {
				audioCodec = track.Codec
				break
			}
		}
	}
	args = append(args, "-map", "0:v:0", "-map", audioMap, "-sn", "-dn")

	if options.Compat != "" {
		return buildCompatPlan(args, probe, mode, options), nil
	}

	webm := mode == ModeRemux && probe.VideoCodec != "h264"
	if mode == ModeRemux {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, options.Encoder.videoArgs()...)
		scaleHeight := 0
		if needsScaling(probe, options) {
			scaleHeight = options.Height
		}
		if filter := options.Encoder.videoFilter(scaleHeight); filter != "" {
			args = append(args, "-vf", filter)
		}
	}

	if webm {
		if audioCodec == "opus" || audioCodec == "vorbis" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "libopus", "-b:a", "128k", "-ac", "2")
		}
		args = append(args, "-f", "webm", "pipe:1")
		return Plan{Mode: mode, ContentType: "video/webm", Args: args, Height: options.Height, Position: options.StartSeconds}, nil
	}

	if audioCodec == "aac" && mode == ModeRemux {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2")
	}
	args = append(args,
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return Plan{Mode: mode, ContentType: "video/mp4", Args: args, Height: options.Height, Position: options.StartSeconds}, nil
}

// buildCompatPlan appends a software VP9/Opus or VP8/Vorbis WebM encode, for
// browsers that cannot decode H.264 or AAC. Encoding is real-time oriented.
func buildCompatPlan(args []string, probe Probe, mode Mode, options Options) Plan {
	if filter := compatScale(probe, options); filter != "" {
		args = append(args, "-vf", filter)
	}
	if options.Compat == "vp9" {
		args = append(args, "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-row-mt", "1", "-crf", "34", "-b:v", "0", "-pix_fmt", "yuv420p",
			"-c:a", "libopus", "-b:a", "128k", "-ac", "2")
	} else {
		args = append(args, "-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-crf", "12", "-b:v", "4M", "-pix_fmt", "yuv420p",
			"-c:a", "libvorbis", "-b:a", "128k", "-ac", "2")
	}
	args = append(args, "-f", "webm", "pipe:1")
	return Plan{Mode: mode, ContentType: "video/webm", Args: args, Height: options.Height, Position: options.StartSeconds}
}

func compatScale(probe Probe, options Options) string {
	if needsScaling(probe, options) {
		return fmt.Sprintf("scale=-2:%d", options.Height)
	}
	return ""
}

// BuildAudioPlan encodes an audio source as 44.1 kHz stereo MPEG-1 Layer III CBR.
// A positive startSeconds begins the stream at that offset, which is how the player seeks in a stream of unknown length.
func BuildAudioPlan(inputPath string, bitrate int, startSeconds float64) (Plan, error) {
	if !ValidAudioBitrate(bitrate) || startSeconds < 0 {
		return Plan{}, ErrInvalidArgs
	}

	rate := strconv.Itoa(bitrate) + "k"
	input := []string{"-hide_banner", "-nostdin"}
	if startSeconds > 0 {
		input = append(input, "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
	}
	return Plan{
		Mode:        ModeTranscode,
		ContentType: "audio/mpeg",
		Position:    startSeconds,
		Args: append(input, []string{
			"-i", inputPath,
			"-map", "0:a:0",
			"-vn", "-sn", "-dn",
			"-c:a", "libmp3lame",
			"-b:a", rate,
			"-minrate", rate,
			"-maxrate", rate,
			"-bufsize", rate,
			"-ar", "44100",
			"-ac", "2",
			"-f", "mp3", "pipe:1",
		}...),
	}, nil
}

type Transcoder struct {
	ffmpegPath  string
	ffprobePath string
	limiter     limiter
	settings    atomic.Pointer[Settings]
	active      atomic.Int64
	sessions    *registry
	logger      *slog.Logger
	logLevel    string
}

// Active returns the number of ffmpeg sessions currently running.
func (t *Transcoder) Active() int { return int(t.active.Load()) }

// writeStall is how long a client may stop reading before its ffmpeg is killed
// and the slot freed. Paused players keep buffering well within this window.
const writeStall = 2 * time.Minute

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
	if maxConcurrent < 0 {
		maxConcurrent = 0
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !ValidLogLevel(logLevel) {
		logLevel = "warning"
	}
	t := &Transcoder{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
		sessions:    newRegistry(),
		logger:      logger,
		logLevel:    logLevel,
	}
	t.limiter.setLimit(maxConcurrent)
	return t, nil
}

type probeOutput struct {
	Streams []struct {
		Index     int    `json:"index"`
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Channels  int    `json:"channels"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Tags      struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
		Disposition struct {
			Default int `json:"default"`
		} `json:"disposition"`
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

	probe, err := parseProbeOutput(stdout.Bytes())
	if err != nil {
		return Probe{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	return probe, nil
}

func parseProbeOutput(data []byte) (Probe, error) {
	var parsed probeOutput
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Probe{}, err
	}
	probe := Probe{AudioTracks: []AudioTrack{}}
	for _, stream := range parsed.Streams {
		switch {
		case stream.CodecType == "video" && probe.VideoCodec == "":
			probe.VideoCodec, probe.Width, probe.Height = stream.CodecName, stream.Width, stream.Height
		case stream.CodecType == "audio":
			if probe.AudioCodec == "" {
				probe.AudioCodec = stream.CodecName
			}
			probe.AudioTracks = append(probe.AudioTracks, AudioTrack{
				Index:    stream.Index,
				Codec:    stream.CodecName,
				Language: stream.Tags.Language,
				Title:    stream.Tags.Title,
				Channels: stream.Channels,
				Default:  stream.Disposition.Default == 1,
			})
		}
	}
	// Audio files often have no cover art, so they only need an audio stream.
	if probe.VideoCodec == "" && probe.AudioCodec == "" {
		return Probe{}, errors.New("media has no video or audio stream")
	}
	probe.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	return probe, nil
}

// KeyframeAtOrBefore returns the position of the last video keyframe at or
// before start, rounded up to 10ms so it can be passed back as a start offset.
// Stream-copy seeks land on a keyframe, so this is where such a stream begins.
func (t *Transcoder) KeyframeAtOrBefore(ctx context.Context, path string, start float64) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	from := math.Max(0, start-60)
	interval := fmt.Sprintf("%.3f%%%.3f", from, start+0.1)
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, t.ffprobePath,
		"-v", "error", "-select_streams", "v:0", "-read_intervals", interval,
		"-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe keyframes: %w", err)
	}
	best, found := 0.0, false
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "K") {
			continue
		}
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err == nil && pts <= start && pts >= best {
			best, found = pts, true
		}
	}
	if !found {
		return 0, errors.New("no keyframe found")
	}
	return math.Ceil(best*100) / 100, nil
}

// Stream runs ffmpeg and copies its output to w. Headers are written only after
// ffmpeg produces its first bytes so a failed start can still return an error.
// Cancelling ctx (client disconnect) kills ffmpeg.
func (t *Transcoder) Stream(parent context.Context, plan Plan, w http.ResponseWriter, head bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	if err := t.limiter.acquire(ctx, slotWait); err != nil {
		return err
	}
	defer t.limiter.release()
	var session *handle
	if !head {
		var err error
		session, err = t.sessions.begin(plan.Meta, plan.Mode, plan.Height, plan.Position, cancel)
		if err != nil {
			return err
		}
		defer session.end()
	}
	// Admin cancellation surfaces as ErrCancelled rather than a client disconnect.
	finish := func(err error) error {
		if session != nil && session.cancelled() {
			return ErrCancelled
		}
		return err
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
	if session != nil {
		session.setPID(cmd.Process.Pid)
	}
	t.logger.Info("ffmpeg started", "pid", cmd.Process.Pid, "mode", string(plan.Mode), "session", plan.Meta.ID, "active", t.active.Add(1), "args", strings.Join(args, " "))
	var waitOnce sync.Once
	var waitErr error
	wait := func() error {
		waitOnce.Do(func() {
			waitErr = cmd.Wait()
			logWriter.Flush()
			t.logger.Info("ffmpeg exited", "pid", cmd.Process.Pid, "duration", time.Since(started).String(), "active", t.active.Add(-1), "error", waitErr)
		})
		return waitErr
	}
	defer func() { _ = wait() }()

	reader := bufio.NewReaderSize(stdout, 64*1024)
	if _, err := reader.Peek(1); err != nil {
		_ = cmd.Process.Kill()
		_ = wait()
		return finish(fmt.Errorf("ffmpeg produced no output: %s", strings.TrimSpace(stderr.String())))
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
	var out io.Writer = w
	var encrypter *EncryptingWriter
	if plan.Encrypt != nil {
		if encrypter, err = NewEncryptingWriter(w, plan.Encrypt.Key, plan.Encrypt.IV); err != nil {
			_ = cmd.Process.Kill()
			return finish(err)
		}
		out = encrypter
	}
	buffer := make([]byte, 64*1024)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			_ = controller.SetWriteDeadline(time.Now().Add(writeStall))
			if _, writeErr := out.Write(buffer[:n]); writeErr != nil {
				_ = cmd.Process.Kill()
				return finish(writeErr)
			}
			session.addBytes(n)
			_ = controller.Flush()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = cmd.Process.Kill()
			return finish(readErr)
		}
	}
	if err := wait(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if encrypter != nil {
		if err := encrypter.Close(); err != nil {
			return finish(err)
		}
	}
	return finish(nil)
}

// Sessions returns the playback sessions that are running or recently active.
func (t *Transcoder) Sessions() []SessionInfo { return t.sessions.list() }

// CancelSession stops a session's ffmpeg processes and blocks it from restarting.
func (t *Transcoder) CancelSession(id string) bool { return t.sessions.cancel(id) }

// MaxConcurrent returns the configured transcode limit; 0 means unlimited.
func (t *Transcoder) MaxConcurrent() int { return t.limiter.limit() }

// SetMaxConcurrent changes the concurrency limit (0 means unlimited) without interrupting running sessions.
func (t *Transcoder) SetMaxConcurrent(n int) { t.limiter.setLimit(n) }

// Settings returns the current video encoder settings.
func (t *Transcoder) Settings() Settings {
	if current := t.settings.Load(); current != nil {
		return *current
	}
	return Settings{}
}

// SetSettings replaces the video encoder settings; sessions already running keep theirs.
func (t *Transcoder) SetSettings(s Settings) { t.settings.Store(&s) }

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
