package httpserver

import (
	"bytes"
	"context"
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
	"strconv"
	"testing"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/flixer"
	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/token"
	"github.com/flixermedia/flixer-streamer/internal/transcode"
)

type testPluginHooks struct {
	result flixer.PluginHookResult
	err    error
	calls  int
}

func (hooks *testPluginHooks) Run(_ context.Context, request flixer.PluginHookRequest) (flixer.PluginHookResult, error) {
	if request.Hook == "authorize" {
		hooks.calls++
	}
	return hooks.result, hooks.err
}

func TestPluginAuthorizationCannotBypassCoreAndFailuresFallBack(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	denied := false
	hooks := &testPluginHooks{result: flixer.PluginHookResult{Allowed: &denied}}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, PluginHooks: hooks})
	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/streamer/stream/invalid", nil))
	if invalid.Code != http.StatusUnauthorized || hooks.calls != 0 {
		t.Fatalf("invalid token status=%d calls=%d", invalid.Code, hooks.calls)
	}
	validToken := createStreamToken(t, secret, "movies", "movie.mp4")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/stream/"+validToken, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("plugin denial status=%d", response.Code)
	}
	hooks.err = context.DeadlineExceeded
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/stream/"+validToken, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("plugin failure must use core decision, status=%d", response.Code)
	}
}

func TestPluginProcessingAndManifestChangesAreBounded(t *testing.T) {
	height := 720
	hooks := &testPluginHooks{result: flixer.PluginHookResult{MaxHeight: &height}}
	handler := &Handler{pluginHooks: hooks, logger: slog.Default()}
	claims := token.Claims{UserID: "user", MediaItemID: "item", PlaySessionID: "session"}
	if got := handler.pluginOptions(context.Background(), claims, transcode.Options{Height: 480}).Height; got != 480 {
		t.Fatalf("plugin raised height: %d", got)
	}
	if got := handler.pluginOptions(context.Background(), claims, transcode.Options{Height: 1080}).Height; got != 720 {
		t.Fatalf("plugin limit not applied: %d", got)
	}
	height = 9999
	if got := handler.pluginOptions(context.Background(), claims, transcode.Options{Height: 1080}).Height; got != 1080 {
		t.Fatalf("invalid height applied: %d", got)
	}
	playlist := "#EXTM3U\n#EXTINF:5,\n0.ts\n"
	start := 3.0
	if got := applyManifestStart(playlist, &start, 10); got != "#EXTM3U\n#EXT-X-START:TIME-OFFSET=3.000,PRECISE=YES\n#EXTINF:5,\n0.ts\n" {
		t.Fatalf("unexpected playlist %q", got)
	}
	start = 11
	if got := applyManifestStart(playlist, &start, 10); got != playlist {
		t.Fatal("invalid start modified the playlist")
	}
}

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
	request := httptest.NewRequest(http.MethodGet, "/streamer/stream/"+token, nil)
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
	for _, url := range []string{"/streamer/stream/", "/streamer/stream/not-a-jwt"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want %d", url, response.Code, http.StatusUnauthorized)
		}

	}

	for _, url := range []string{"/stream/not-a-jwt", "/healthz"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("legacy route GET %s status = %d, want %d", url, response.Code, http.StatusNotFound)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	t.Setenv("APP_VERSION", "1.2.3")
	handler := NewHandler(Options{
		Logger: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/healthz", nil))
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
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/streamer/admin/sessions", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", denied.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/streamer/admin/sessions", nil)
	request.Header.Set("Authorization", "Bearer "+AdminKey(secret))
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, request)
	if allowed.Code != http.StatusOK || !bytes.Contains(allowed.Body.Bytes(), []byte(`"sessions":[]`)) {
		t.Fatalf("valid key: %d %s", allowed.Code, allowed.Body)
	}

	del := httptest.NewRequest(http.MethodDelete, "/streamer/admin/sessions/missing", nil)
	del.Header.Set("Authorization", "Bearer "+AdminKey(secret))
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, del)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown session: status = %d, want 404", missing.Code)
	}
}

func TestAdminTranscodeCleanupRemovesOnlyExpiredFiles(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	cache := t.TempDir()
	oldPath := filepath.Join(cache, "old.ts")
	recentPath := filepath.Join(cache, "recent.ts")
	outsidePath := filepath.Join(t.TempDir(), "outside.ts")
	for _, path := range []string{oldPath, recentPath, outsidePath} {
		if err := os.WriteFile(path, []byte("cache"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(cache, "outside-link")); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, TranscodeDir: cache})

	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "/streamer/admin/transcode-cache/cleanup", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status = %d, want %d", denied.Code, http.StatusUnauthorized)
	}

	request := httptest.NewRequest(http.MethodPost, "/streamer/admin/transcode-cache/cleanup", nil)
	request.Header.Set("Authorization", "Bearer "+AdminKey(secret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"removed\":1}\n" {
		t.Fatalf("cleanup response = %d %q", response.Code, response.Body.String())
	}
	for _, path := range []string{oldPath, recentPath, outsidePath} {
		_, err := os.Stat(path)
		if path == oldPath && !os.IsNotExist(err) {
			t.Errorf("expired file still exists (err = %v)", err)
		}
		if path != oldPath && err != nil {
			t.Errorf("preserved file %q is unavailable: %v", path, err)
		}
	}
}

func TestParseAudioTrackOption(t *testing.T) {
	options, err := parseOptions(httptest.NewRequest(http.MethodGet, "/?audioTrack=3", nil))
	if err != nil {
		t.Fatal(err)
	}
	if options.AudioStreamIndex == nil || *options.AudioStreamIndex != 3 {
		t.Fatalf("audio stream index = %v, want 3", options.AudioStreamIndex)
	}
	for _, value := range []string{"-1", "invalid"} {
		if _, err := parseOptions(httptest.NewRequest(http.MethodGet, "/?audioTrack="+value, nil)); err == nil {
			t.Errorf("audioTrack=%s should be rejected", value)
		}
	}
}

func TestParseCompatOption(t *testing.T) {
	for _, value := range []string{"mp3", "vp8", "vp9"} {
		options, err := parseOptions(httptest.NewRequest(http.MethodGet, "/?compat="+value, nil))
		if err != nil || options.Compat != value {
			t.Errorf("compat=%s: options = %+v, error = %v", value, options, err)
		}
	}
	if _, err := parseOptions(httptest.NewRequest(http.MethodGet, "/?compat=invalid", nil)); err == nil {
		t.Fatal("invalid compatibility profile should be rejected")
	}
}

func TestParseAudioBitrateOption(t *testing.T) {
	for _, bitrate := range []int{128, 160, 192, 224, 256, 320} {
		options, err := parseOptions(httptest.NewRequest(http.MethodGet, "/?audioBitrate="+strconv.Itoa(bitrate), nil))
		if err != nil {
			t.Errorf("bitrate %d: %v", bitrate, err)
			continue
		}
		if options.AudioBitrateKbps != bitrate {
			t.Errorf("bitrate = %d, want %d", options.AudioBitrateKbps, bitrate)
		}
	}
	for _, value := range []string{"127", "129", "321", "invalid"} {
		if _, err := parseOptions(httptest.NewRequest(http.MethodGet, "/?audioBitrate="+value, nil)); err == nil {
			t.Errorf("audioBitrate=%s should be rejected", value)
		}
	}
}

var denyDirect bool

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
		Direct:        !denyDirect,
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
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/lyrics/"+tok, nil))
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

func TestStreamRefusesOriginalBytesWithoutDirectClaim(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.flac"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "music", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, Logger: slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))})

	denyDirect = true
	defer func() { denyDirect = false }()
	request := httptest.NewRequest(http.MethodGet, "/streamer/stream/"+createStreamToken(t, secret, "music", "song.flac"), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", response.Code, response.Body)
	}
}
