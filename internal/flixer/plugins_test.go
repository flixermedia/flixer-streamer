package flixer

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPluginHooksSignAndValidateRequests(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		message := pluginConfigContext
		if r.Method == http.MethodPost {
			hash := sha256.Sum256(body)
			message = pluginHookContext + ":" + hex.EncodeToString(hash[:])
		}
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(message + ":" + r.Header.Get("X-Streamer-Timestamp")))
		if r.Header.Get("X-Streamer-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("invalid request signature")
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"hooks":["authorize"]}`))
		} else {
			_, _ = w.Write([]byte(`{"allowed":false}`))
		}
	}))
	defer server.Close()
	hooks := NewPluginHooks(server.URL, secret, nil)
	if err := hooks.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := hooks.Run(context.Background(), PluginHookRequest{Hook: "authorize", UserID: "u", ItemID: "i", SessionID: "s"})
	if err != nil || result.Allowed == nil || *result.Allowed {
		t.Fatalf("unexpected hook result: %+v, %v", result, err)
	}
}
