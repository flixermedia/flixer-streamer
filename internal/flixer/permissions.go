package flixer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const permissionErrorPath = "/api/indexer/permission-error"

// PermissionReporter forwards filesystem permission failures to flixer so admins are notified.
type PermissionReporter struct {
	baseURL string
	secret  []byte
	client  *http.Client
	logger  *slog.Logger
}

// NewPermissionReporter creates a reporter targeting flixer's internal permission-error endpoint.
func NewPermissionReporter(baseURL string, secret []byte, logger *slog.Logger) *PermissionReporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &PermissionReporter{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  secret,
		client:  &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		logger:  logger,
	}
}

// Report sends a signed permission error alert to flixer.
func (r *PermissionReporter) Report(ctx context.Context, operation, path string, err error) {
	if r == nil || r.baseURL == "" || len(r.secret) < 32 || err == nil {
		return
	}
	body, marshalErr := json.Marshal(struct {
		Source    string `json:"source"`
		Operation string `json:"operation,omitempty"`
		Path      string `json:"path,omitempty"`
		Message   string `json:"message"`
	}{
		Source:    "flixer-streamer",
		Operation: operation,
		Path:      path,
		Message:   err.Error(),
	})
	if marshalErr != nil {
		return
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+permissionErrorPath, bytes.NewReader(body))
	if reqErr != nil {
		return
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := sha256.Sum256(body)
	payload := fmt.Sprintf("flixer-indexer:v1:%s:POST:%s:%s", timestamp, permissionErrorPath, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, r.secret)
	_, _ = mac.Write([]byte(payload))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-indexer-timestamp", timestamp)
	req.Header.Set("x-indexer-signature", hex.EncodeToString(mac.Sum(nil)))

	resp, doErr := r.client.Do(req)
	if doErr != nil {
		r.logger.Warn("could not report permission error to flixer", "operation", operation, "path", path, "error", doErr)
		return
	}
	_ = resp.Body.Close()
}
