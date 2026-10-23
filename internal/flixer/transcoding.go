package flixer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

const (
	transcodingPath    = "/api/streamer/transcoding"
	transcodingContext = "flixer-streamer:transcoding:v1"
)

// TranscodingSettings are the encoder choices an admin makes in flixer. MaxConcurrent is nil when flixer leaves it
// to the streamer's MAX_TRANSCODES environment variable.
type TranscodingSettings struct {
	Encoder       transcode.Settings
	MaxConcurrent *int
}

type transcodingResponse struct {
	HWAccel        string `json:"hwAccel"`
	Preset         string `json:"preset"`
	CRF            int    `json:"crf"`
	Threads        int    `json:"threads"`
	MaxBitrateKbps int    `json:"maxBitrateKbps"`
	MaxConcurrent  *int   `json:"maxConcurrent"`
}

// FetchTranscoding asks flixer for the transcoding settings, signed like FetchRoots.
func FetchTranscoding(ctx context.Context, client *http.Client, baseURL string, secret []byte, now time.Time) (TranscodingSettings, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+transcodingPath, nil)
	if err != nil {
		return TranscodingSettings{}, err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(transcodingContext + ":" + timestamp))
	req.Header.Set("X-Streamer-Timestamp", timestamp)
	req.Header.Set("X-Streamer-Signature", hex.EncodeToString(mac.Sum(nil)))

	resp, err := client.Do(req)
	if err != nil {
		return TranscodingSettings{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TranscodingSettings{}, fmt.Errorf("flixer returned status %d", resp.StatusCode)
	}

	var body transcodingResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return TranscodingSettings{}, fmt.Errorf("decode transcoding settings: %w", err)
	}
	return TranscodingSettings{
		Encoder:       transcode.Settings{HWAccel: body.HWAccel, Preset: body.Preset, CRF: body.CRF, Threads: body.Threads, MaxBitrateKbps: body.MaxBitrateKbps},
		MaxConcurrent: body.MaxConcurrent,
	}, nil
}

// SyncTranscoding applies flixer's transcoding settings to the transcoder until ctx ends, so changes made in the
// admin dashboard take effect within RefreshInterval without restarting the streamer.
func SyncTranscoding(ctx context.Context, transcoder *transcode.Transcoder, baseURL string, secret []byte, logger *slog.Logger) {
	client := &http.Client{Timeout: requestTimeout}
	synced := false
	for {
		settings, err := FetchTranscoding(ctx, client, baseURL, secret, time.Now())
		wait := retryInterval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !synced {
				logger.Warn("could not fetch transcoding settings from flixer; retrying", "error", err)
			} else {
				logger.Warn("could not refresh transcoding settings from flixer; keeping the previous ones", "error", err)
			}
		} else {
			if transcoder.Settings() != settings.Encoder {
				logger.Info("transcoding settings updated", "hwAccel", settings.Encoder.HWAccel, "preset", settings.Encoder.Preset, "crf", settings.Encoder.CRF, "threads", settings.Encoder.Threads, "maxBitrateKbps", settings.Encoder.MaxBitrateKbps)
			}
			transcoder.SetSettings(settings.Encoder)
			if settings.MaxConcurrent != nil {
				transcoder.SetMaxConcurrent(*settings.MaxConcurrent)
			}
			synced = true
			wait = RefreshInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
