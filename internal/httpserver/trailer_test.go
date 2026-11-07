package httpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

func TestTrailerURLValidation(t *testing.T) {
	for _, test := range []struct {
		url  string
		want bool
	}{
		{"https://r1.googlevideo.com/videoplayback?x=1", true},
		{"http://r1.googlevideo.com/video", false},
		{"https://googlevideo.com.attacker.test/video", false},
		{"https://127.0.0.1/video", false},
		{"https://r1.googlevideo.com:8443/video", false},
		{"https://user@r1.googlevideo.com/video", false},
	} {
		if got := validTrailerURL(test.url); got != test.want {
			t.Errorf("validTrailerURL(%q) = %v, want %v", test.url, got, test.want)
		}
	}
}

func TestTrailerProxyRealUpstream(t *testing.T) {
	target := os.Getenv("FLIXER_TEST_TRAILER_URL")
	if target == "" {
		t.Skip("set FLIXER_TEST_TRAILER_URL to run the real-upstream smoke test")
	}
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	raw := createIPTVToken(t, secret, token.Locator{Type: "trailer", URL: target}, time.Now().Add(time.Hour))
	handler := NewHandler(Options{Secret: secret})
	request := httptest.NewRequest(http.MethodGet, "/streamer/trailer/"+raw, nil)
	request.Header.Set("Range", "bytes=0-1023")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.Len() != 1024 {
		t.Fatalf("upstream range returned status %d and %d bytes", response.Code, response.Body.Len())
	}
}

func TestTrailerProxyForwardsRangeAndStreamsResponse(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	raw := createIPTVToken(t, secret, token.Locator{Type: "trailer", URL: "https://r1.googlevideo.com/video"}, time.Now().Add(time.Hour))
	h := &Handler{secret: secret, live: newLiveProxy()}
	request := httptest.NewRequest(http.MethodGet, "/trailer/"+raw, nil)
	request.Header.Set("Range", "bytes=1-3")
	response := httptest.NewRecorder()
	h.serveTrailer(response, request, func(_ context.Context, target string, headers http.Header) (*http.Response, error) {
		if target != "https://r1.googlevideo.com/video" || headers.Get("Range") != "bytes=1-3" {
			t.Fatalf("unexpected upstream target or range")
		}
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": []string{"bytes 1-3/4"}, "Content-Length": []string{"3"}},
			Body:       io.NopCloser(strings.NewReader("bcd")),
		}, nil
	})
	if response.Code != http.StatusPartialContent || response.Body.String() != "bcd" || response.Header().Get("Content-Range") != "bytes 1-3/4" {
		t.Fatalf("unexpected proxy response: %d %q %v", response.Code, response.Body.String(), response.Header())
	}
}

func TestTrailerEndpointRejectsWrongLocatorAndInvalidToken(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	handler := NewHandler(Options{Secret: secret})
	for _, locator := range []token.Locator{
		{Type: "iptv", URL: "https://r1.googlevideo.com/video"},
		{Type: "trailer", URL: "https://127.0.0.1/video"},
	} {
		raw := createIPTVToken(t, secret, locator, time.Now().Add(time.Hour))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/trailer/"+raw, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/trailer/not-a-token", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", response.Code)
	}
}

func TestTrailerLocatorRejectedByOtherEndpoints(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	raw := createIPTVToken(t, secret, token.Locator{Type: "trailer", URL: "https://r1.googlevideo.com/video"}, time.Now().Add(time.Hour))
	handler := NewHandler(Options{Secret: secret})
	for _, path := range []string{"/streamer/stream/", "/streamer/live/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+raw, nil))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", path, response.Code)
		}
	}
}
