package exporter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestControlClientCanWaitForPublishedSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"endpoint":"https://logger.example","token":"` + strings.Repeat("x", 43) + `","expiresAt":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()
	available := false
	client, err := NewControlClientWithSecretProvider(server.URL, "updater", func() ([]byte, error) {
		if !available {
			return nil, errors.New("not published")
		}
		return []byte(strings.Repeat("s", 32)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Fetch(context.Background()); err == nil {
		t.Fatal("expected unavailable secret to reject enrollment")
	}
	available = true
	if _, err := client.Fetch(context.Background()); err != nil {
		t.Fatalf("enrollment did not recover: %v", err)
	}
}
