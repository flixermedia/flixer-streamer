package exporter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControlClientSignsAndValidates(t *testing.T) {
	secret := strings.Repeat("s", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/logger/indexer" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		empty := sha256.Sum256(nil)
		mac := hmac.New(sha256.New, []byte(secret))
		fmt.Fprintf(mac, "flixer-indexer:v1:%s:GET:%s:%x", r.Header.Get("X-Indexer-Timestamp"), r.URL.Path, empty)
		if r.Header.Get("X-Indexer-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("invalid control signature")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"endpoint": "https://logger.example", "token": strings.Repeat("x", 43),
			"expiresAt": time.Now().Add(time.Hour),
		})
	}))
	defer server.Close()
	client, err := NewControlClient(server.URL, "indexer", []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Fetch(context.Background())
	if err != nil || got.Endpoint != "https://logger.example" || got.Token != strings.Repeat("x", 43) {
		t.Fatalf("fetch failed: endpoint=%q, error=%v", got.Endpoint, err)
	}
}

func TestUnixControlClient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 32)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		empty := sha256.Sum256(nil)
		mac := hmac.New(sha256.New, []byte(secret))
		fmt.Fprintf(mac, "flixer-indexer:v1:%s:GET:%s:%x", r.Header.Get("X-Indexer-Timestamp"), r.URL.Path, empty)
		if r.URL.Path != "/api/internal/logger/streamer" || r.Header.Get("X-Indexer-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("invalid control request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"endpoint": "https://logger.example", "token": strings.Repeat("x", 43),
			"expiresAt": time.Now().Add(time.Hour),
		})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client, err := NewUnixControlClient(path, "streamer", func() ([]byte, error) { return []byte(secret), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Fetch(context.Background()); err != nil {
		t.Fatalf("socket fetch failed: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Fetch(context.Background()); err == nil {
		t.Fatal("accepted socket in unsafe directory")
	}
	if _, err := NewUnixControlClient("relative.sock", "streamer", func() ([]byte, error) { return []byte(secret), nil }); err == nil {
		t.Fatal("accepted relative socket path")
	}
}

func TestControlClientRejectsUnsafeConfiguration(t *testing.T) {
	for _, origin := range []string{"http://example.com", "http://localhost:8080", "https://user@example.com", "https://example.com/?secret=x"} {
		if _, err := NewControlClient(origin, "indexer", []byte(strings.Repeat("s", 32))); err == nil {
			t.Fatalf("accepted unsafe origin %q", origin)
		}
	}
	if _, err := NewControlClient("https://example.com", "unknown", []byte(strings.Repeat("s", 32))); err == nil {
		t.Fatal("accepted unknown producer")
	}
}
