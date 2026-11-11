package flixer

import (
	"bytes"
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
	"sync/atomic"
	"time"
)

const pluginConfigContext = "flixer-streamer:plugins:v1"
const pluginHookContext = "flixer-streamer:plugin-hooks:v1"

type PluginHookRequest struct {
	Hook            string  `json:"hook"`
	UserID          string  `json:"userId"`
	ItemID          string  `json:"itemId"`
	SessionID       string  `json:"sessionId"`
	Height          int     `json:"height,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
}

type PluginHookResult struct {
	Allowed      *bool    `json:"allowed"`
	MaxHeight    *int     `json:"maxHeight"`
	StartSeconds *float64 `json:"startSeconds"`
}

type PluginHooks struct {
	baseURL string
	secret  []byte
	client  *http.Client
	logger  *slog.Logger
	active  atomic.Value
}

func NewPluginHooks(baseURL string, secret []byte, logger *slog.Logger) *PluginHooks {
	if logger == nil {
		logger = slog.Default()
	}
	hooks := &PluginHooks{baseURL: strings.TrimRight(baseURL, "/"), secret: secret, client: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, logger: logger}
	hooks.active.Store(map[string]bool{})
	return hooks
}

func (hooks *PluginHooks) Enabled(name string) bool {
	return hooks.active.Load().(map[string]bool)[name]
}

func (hooks *PluginHooks) sign(request *http.Request, context string, body []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	message := context
	if body != nil {
		digest := sha256.Sum256(body)
		message += ":" + hex.EncodeToString(digest[:])
	}
	mac := hmac.New(sha256.New, hooks.secret)
	_, _ = mac.Write([]byte(message + ":" + timestamp))
	request.Header.Set("X-Streamer-Timestamp", timestamp)
	request.Header.Set("X-Streamer-Signature", hex.EncodeToString(mac.Sum(nil)))
}

func (hooks *PluginHooks) refresh(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, hooks.baseURL+"/api/streamer/plugins", nil)
	if err != nil {
		return err
	}
	hooks.sign(request, pluginConfigContext, nil)
	response, err := hooks.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("plugin configuration returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
		return err
	}
	if result.Hooks == nil {
		return fmt.Errorf("invalid plugin configuration")
	}
	active := make(map[string]bool)
	for _, hook := range result.Hooks {
		if hook == "authorize" || hook == "manifest" || hook == "processing" {
			active[hook] = true
		}
	}
	hooks.active.Store(active)
	return nil
}

func (hooks *PluginHooks) Sync(ctx context.Context) {
	for {
		if err := hooks.refresh(ctx); err != nil && ctx.Err() == nil {
			hooks.logger.Warn("could not refresh streamer plugin configuration", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func (hooks *PluginHooks) Run(ctx context.Context, payload PluginHookRequest) (PluginHookResult, error) {
	if !hooks.Enabled(payload.Hook) {
		return PluginHookResult{}, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return PluginHookResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, hooks.baseURL+"/api/streamer/plugin-hooks", bytes.NewReader(body))
	if err != nil {
		return PluginHookResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	hooks.sign(request, pluginHookContext, body)
	response, err := hooks.client.Do(request)
	if err != nil {
		return PluginHookResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return PluginHookResult{}, fmt.Errorf("plugin hook returned HTTP %d", response.StatusCode)
	}
	var result PluginHookResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
		return PluginHookResult{}, err
	}
	return result, nil
}
