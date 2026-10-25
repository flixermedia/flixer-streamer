package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/token"
)

func TestTrickplayServesIndexAndSheetWithValidToken(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	locator := token.Locator{RootID: "movies", RelativePath: "folder/movie.mp4"}
	if got := trickplayKey(locator); got != "3c47b4b1a186d47b47783e012c0df2ab" {
		t.Fatalf("trickplay key = %q", got)
	}
	dir := t.TempDir()
	keyDir := filepath.Join(dir, trickplayKey(locator))
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	index := []byte(`{"version":1,"count":1}`)
	sheet := []byte("jpeg-data")
	if err := os.WriteFile(filepath.Join(keyDir, "index.json"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "0.jpg"), sheet, 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, TrickplayDir: dir})
	streamToken := createStreamToken(t, secret, locator.RootID, locator.RelativePath)
	for _, test := range []struct {
		method, name, contentType, cache string
		body                             []byte
	}{{http.MethodGet, "index.json", "application/json", "private, max-age=300", index}, {http.MethodGet, "0.jpg", "image/jpeg", "private, max-age=86400", sheet}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, "/trickplay/"+streamToken+"/"+test.name, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != test.contentType || response.Header().Get("Cache-Control") != test.cache || !bytes.Equal(response.Body.Bytes(), test.body) {
			t.Errorf("%s response = %d, headers %v, body %q", test.name, response.Code, response.Header(), response.Body.Bytes())
		}
	}
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/trickplay/"+streamToken+"/0.jpg", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD response = %d, body %q", head.Code, head.Body.String())
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, "/trickplay/"+streamToken+"/0.jpg", nil)
	rangeRequest.Header.Set("Range", "bytes=0-3")
	ranged := httptest.NewRecorder()
	handler.ServeHTTP(ranged, rangeRequest)
	if ranged.Code != http.StatusPartialContent || ranged.Header().Get("Content-Range") != "bytes 0-3/9" || ranged.Body.String() != "jpeg" {
		t.Errorf("range response = %d, Content-Range %q, body %q", ranged.Code, ranged.Header().Get("Content-Range"), ranged.Body.String())
	}
}

func TestTrickplayMissingTraversalAndBadToken(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	locator := token.Locator{RootID: "movies", RelativePath: "movie.mp4"}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, TrickplayDir: t.TempDir()})
	valid := createStreamToken(t, secret, locator.RootID, locator.RelativePath)
	for _, test := range []struct {
		url  string
		code int
		want string
	}{{"/trickplay/" + valid + "/9.jpg", http.StatusNotFound, "trickplay_not_available"}, {"/trickplay/" + valid + "/../index.json", http.StatusNotFound, "trickplay_not_available"}, {"/trickplay/not-a-token/index.json", http.StatusUnauthorized, "invalid_token"}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.url, nil))
		var body map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if response.Code != test.code || body["error"] != test.want {
			t.Errorf("GET %s = %d %v, want %d code %q", test.url, response.Code, body, test.code, test.want)
		}
	}
}

func TestTrickplayRejectsUnsupportedMethod(t *testing.T) {
	handler := NewHandler(Options{Secret: []byte("test-secret-that-is-at-least-thirty-two-bytes")})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/trickplay/no/index.json", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST response = %d, Allow %q", response.Code, response.Header().Get("Allow"))
	}
}
