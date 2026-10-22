package flixer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchRootsSignsRequestAndParsesRoots(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/streamer/media-roots" {
			t.Errorf("path = %q", r.URL.Path)
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte("flixer-streamer:media-roots:v1:" + r.Header.Get("X-Streamer-Timestamp")))
		if r.Header.Get("X-Streamer-Signature") != hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("X-Streamer-Timestamp") != "1700000000" {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"roots":[{"id":"root-a","path":"/media"},{"id":"root-b","path":"/more"}]}`))
	}))
	defer server.Close()

	roots, err := FetchRoots(context.Background(), server.Client(), server.URL+"/", secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0].ID != "root-a" || roots[1].Path != "/more" {
		t.Fatalf("roots = %#v", roots)
	}

	if _, err := FetchRoots(context.Background(), server.Client(), server.URL, []byte("wrong-secret-wrong-secret-wrong-"), now); err == nil {
		t.Fatal("expected an error for a bad signature")
	}
}
