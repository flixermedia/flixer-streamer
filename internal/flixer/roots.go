// Package flixer fetches configuration owned by flixer, the streamer's media roots.
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

	"github.com/flixermedia/flixer-streamer/internal/media"
)

const (
	rootsPath      = "/api/streamer/media-roots"
	signingContext = "flixer-streamer:media-roots:v1"
	requestTimeout = 5 * time.Second
	retryInterval  = 2 * time.Second
	// RefreshInterval is how often the roots are re-read once flixer has answered.
	RefreshInterval = time.Minute
)

type rootsResponse struct {
	Roots []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"roots"`
}

// FetchRoots asks flixer for the root media folders. The request is signed
// with the shared token secret so only a streamer holding it is answered.
func FetchRoots(ctx context.Context, client *http.Client, baseURL string, secret []byte, now time.Time) ([]media.Root, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+rootsPath, nil)
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingContext + ":" + timestamp))
	req.Header.Set("X-Streamer-Timestamp", timestamp)
	req.Header.Set("X-Streamer-Signature", hex.EncodeToString(mac.Sum(nil)))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flixer returned status %d", resp.StatusCode)
	}

	var body rootsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode media roots: %w", err)
	}
	roots := make([]media.Root, 0, len(body.Roots))
	for _, root := range body.Roots {
		roots = append(roots, media.Root{ID: root.ID, Path: root.Path})
	}
	return roots, nil
}

// SyncRoots keeps the resolver's roots in step with flixer until ctx ends. It
// retries quickly until flixer first answers, then refreshes periodically so
// roots changed in flixer apply without restarting the streamer.
func SyncRoots(ctx context.Context, resolver *media.Resolver, baseURL string, secret []byte, logger *slog.Logger) {
	client := &http.Client{Timeout: requestTimeout}
	synced := false
	for {
		roots, err := FetchRoots(ctx, client, baseURL, secret, time.Now())
		wait := retryInterval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !synced {
				logger.Warn("could not fetch media roots from flixer; retrying", "error", err)
			} else {
				logger.Warn("could not refresh media roots from flixer; keeping the previous ones", "error", err)
			}
		} else {
			if updateErr := resolver.Update(roots); updateErr != nil {
				logger.Warn("some media roots from flixer are unusable", "error", updateErr)
			}
			if !synced {
				logger.Info("media roots loaded from flixer", "count", len(roots))
			}
			synced = true
		}
		if synced {
			wait = RefreshInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
