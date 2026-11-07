package exporter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ControlClient obtains producer credentials over TLS or literal loopback.
// The shared Flixer application secret authenticates the request but is not a
// distinct per-service identity.
type ControlClient struct {
	origin string
	path   string
	secret func() ([]byte, error)
	client *http.Client
}

// NewUnixControlClient uses a shared, protected local socket instead of
// transmitting credentials across a container network.
func NewUnixControlClient(path, service string, secret func() ([]byte, error)) (*ControlClient, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("logger control socket path must be absolute")
	}
	client, err := NewControlClientWithSecretProvider("http://127.0.0.1", service, secret)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dir, err := os.Lstat(filepath.Dir(path))
			if err != nil || !dir.IsDir() || dir.Mode().Perm()&0o022 != 0 {
				return nil, errors.New("logger control socket directory is unavailable or unsafe")
			}
			file, err := os.Lstat(path)
			if err != nil || file.Mode()&os.ModeSocket == 0 {
				return nil, errors.New("logger control socket is unavailable")
			}
			if !socketOwnedByDirectory(dir, file) {
				return nil, errors.New("logger control socket owner does not match its directory")
			}
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	client.client.Transport = transport
	return client, nil
}

func NewControlClient(origin, service string, secret []byte) (*ControlClient, error) {
	copyOfSecret := append([]byte(nil), secret...)
	if len(copyOfSecret) < 32 {
		return nil, errors.New("Flixer application secret is too short")
	}
	return NewControlClientWithSecretProvider(origin, service, func() ([]byte, error) { return copyOfSecret, nil })
}

// NewControlClientWithSecretProvider permits late publication and rotation of
// the shared secret without waiting during service startup.
func NewControlClientWithSecretProvider(origin, service string, secret func() ([]byte, error)) (*ControlClient, error) {
	switch service {
	case "indexer", "streamer", "updater":
	default:
		return nil, errors.New("invalid logger producer")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return nil, errors.New("Flixer endpoint must be an origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("Flixer endpoint requires HTTPS except on loopback")
		}
	}
	if secret == nil {
		return nil, errors.New("Flixer application secret provider is required")
	}
	return &ControlClient{
		origin: strings.TrimRight(u.String(), "/"),
		path:   "/api/internal/logger/" + service,
		secret: secret,
		client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (c *ControlClient) Fetch(ctx context.Context) (Credential, error) {
	secret, err := c.secret()
	if err != nil || len(secret) < 32 {
		return Credential{}, errors.New("Flixer application secret is unavailable")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	empty := sha256.Sum256(nil)
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "flixer-indexer:v1:%s:GET:%s:%x", timestamp, c.path, empty)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+c.path, nil)
	if err != nil {
		return Credential{}, errors.New("cannot create logger enrollment request")
	}
	req.Header.Set("X-Indexer-Timestamp", timestamp)
	req.Header.Set("X-Indexer-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := c.client.Do(req)
	if err != nil {
		return Credential{}, errors.New("logger enrollment request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Credential{}, fmt.Errorf("logger enrollment rejected: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 {
		return Credential{}, errors.New("invalid logger enrollment response")
	}
	var response struct {
		Endpoint  string    `json:"endpoint"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Credential{}, errors.New("invalid logger enrollment response")
	}
	if response.Endpoint == "" || !validToken(response.Token) || !response.ExpiresAt.After(time.Now()) {
		return Credential{}, errors.New("invalid logger enrollment credential")
	}
	return Credential(response), nil
}
