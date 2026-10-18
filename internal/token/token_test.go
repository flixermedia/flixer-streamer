package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	valid := signedToken(t, secret, Claims{
		Issuer:        Issuer,
		Audience:      Audience,
		Subject:       "user-1",
		UserID:        "user-1",
		MediaItemID:   "item-1",
		MediaSourceID: "source-1",
		PlaySessionID: "session-1",
		MediaLocator:  "opaque",
		IssuedAt:      now.Unix(),
		ExpiresAt:     now.Add(time.Hour).Unix(),
		TokenID:       "token-1",
	})

	if _, err := Validate(valid, secret, now); err != nil {
		t.Fatalf("Validate(valid token) error = %v", err)
	}

	parts := strings.Split(valid, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	tampered := strings.Join(parts, ".")

	tests := []struct {
		name  string
		token string
	}{
		{name: "tampered signature", token: tampered},
		{name: "wrong audience", token: signedToken(t, secret, Claims{
			Issuer: Issuer, Audience: "other", Subject: "user-1", UserID: "user-1",
			MediaItemID: "item-1", MediaSourceID: "source-1", PlaySessionID: "session-1",
			MediaLocator: "opaque", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
			TokenID: "token-1",
		})},
		{name: "expired", token: signedToken(t, secret, Claims{
			Issuer: Issuer, Audience: Audience, Subject: "user-1", UserID: "user-1",
			MediaItemID: "item-1", MediaSourceID: "source-1", PlaySessionID: "session-1",
			MediaLocator: "opaque", IssuedAt: now.Add(-2 * time.Hour).Unix(),
			ExpiresAt: now.Add(-time.Second).Unix(), TokenID: "token-1",
		})},
		{name: "excessive lifetime", token: signedToken(t, secret, Claims{
			Issuer: Issuer, Audience: Audience, Subject: "user-1", UserID: "user-1",
			MediaItemID: "item-1", MediaSourceID: "source-1", PlaySessionID: "session-1",
			MediaLocator: "opaque", IssuedAt: now.Unix(),
			ExpiresAt: now.Add(MaxTokenAge + time.Second).Unix(), TokenID: "token-1",
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Validate(test.token, secret, now); err == nil {
				t.Fatal("Validate() succeeded, want an error")
			}
		})
	}
}

func signedToken(t *testing.T, secret []byte, claims Claims) string {
	t.Helper()
	headerBytes, err := json.Marshal(header{Algorithm: "HS256", Type: "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerBytes)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsBytes)
	unsigned := encodedHeader + "." + encodedClaims
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
