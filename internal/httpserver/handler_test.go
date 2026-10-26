package httpserver

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/token"
)

func TestStreamServesByteRangeWithValidToken(t *testing.T) {
	root := t.TempDir()
	content := []byte("0123456789")
	if err := os.WriteFile(filepath.Join(root, "movie.mp4"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	handler := NewHandler(Options{
		Secret:   secret,
		Resolver: resolver,
		Logger:   slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)),
	})

	token := createStreamToken(t, secret, "movies", "movie.mp4")
	request := httptest.NewRequest(http.MethodGet, "/stream/"+token, nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusPartialContent, response.Body)
	}
	if got := response.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 2-5/10")
	}
	if got := response.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := response.Body.String(); got != "2345" {
		t.Errorf("body = %q, want 2345", got)
	}
}

func TestStreamRejectsMissingOrInvalidToken(t *testing.T) {
	handler := NewHandler(Options{
		Secret: []byte("test-secret-that-is-at-least-thirty-two-bytes"),
		Logger: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)),
	})
	for _, url := range []string{"/stream/", "/stream/not-a-jwt"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want %d", url, response.Code, http.StatusUnauthorized)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	t.Setenv("APP_VERSION", "1.2.3")
	handler := NewHandler(Options{
		Logger: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || response.Body.String() != "{\"activeTranscodes\":0,\"status\":\"ok\",\"version\":\"1.2.3\"}\n" {
		t.Fatalf("health response = %d %q", response.Code, response.Body.String())
	}
}

func TestAdminSessionsRequireKey(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	handler := NewHandler(Options{
		Secret: secret,
		Logger: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)),
	})

	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/admin/sessions", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", denied.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/sessions", nil)
	request.Header.Set("Authorization", "Bearer "+AdminKey(secret))
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, request)
	if allowed.Code != http.StatusOK || !bytes.Contains(allowed.Body.Bytes(), []byte(`"sessions":[]`)) {
		t.Fatalf("valid key: %d %s", allowed.Code, allowed.Body)
	}

	del := httptest.NewRequest(http.MethodDelete, "/admin/sessions/missing", nil)
	del.Header.Set("Authorization", "Bearer "+AdminKey(secret))
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, del)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown session: status = %d, want 404", missing.Code)
	}
}

func createStreamToken(t *testing.T, secret []byte, rootID, relativePath string) string {
	t.Helper()
	locator, err := json.Marshal(token.Locator{RootID: rootID, RelativePath: relativePath})
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256(append([]byte("flixer-streamer:media-locator:v1\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	encrypted := gcm.Seal(nonce, nonce, locator, []byte("flixer-streamer:media-locator:v1"))
	mediaLocator := base64.RawURLEncoding.EncodeToString(encrypted)

	now := time.Now().Unix()
	claims, err := json.Marshal(token.Claims{
		Issuer:        token.Issuer,
		Audience:      token.Audience,
		Subject:       "user-1",
		UserID:        "user-1",
		MediaItemID:   "item-1",
		MediaSourceID: "source-1",
		PlaySessionID: "session-1",
		MediaLocator:  mediaLocator,
		IssuedAt:      now,
		ExpiresAt:     now + 3600,
		TokenID:       "token-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestLyricsPreferStoredCopyOverSidecar(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{filepath.Join(root, "song.flac"): "audio", filepath.Join(root, "song.lrc"): "sidecar"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "music", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, LyricsDir: store, Logger: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))})
	tok := createStreamToken(t, secret, "music", "song.flac")

	get := func() string {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/lyrics/"+tok, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", response.Code, response.Body)
		}
		return response.Body.String()
	}
	if got := get(); got != "sidecar" {
		t.Fatalf("without a stored copy got %q", got)
	}
	if err := os.WriteFile(filepath.Join(store, "item-1.lrc"), []byte("stored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "stored" {
		t.Fatalf("with a stored copy got %q", got)
	}
}
