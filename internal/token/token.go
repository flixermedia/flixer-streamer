package token

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	Issuer       = "flixer"
	Audience     = "flixer-streamer"
	MaxTokenAge  = 4 * time.Hour
	clockSkew    = 30 * time.Second
	maxTokenSize = 16 * 1024
	locatorAAD   = "flixer-streamer:media-locator:v1"
)

type Claims struct {
	Issuer              string   `json:"iss"`
	Audience            string   `json:"aud"`
	Subject             string   `json:"sub"`
	UserID              string   `json:"userId"`
	MediaItemID         string   `json:"mediaItemId"`
	MediaSourceID       string   `json:"mediaSourceId"`
	PlaySessionID       string   `json:"playSessionId"`
	MediaLocator        string   `json:"mediaLocator"`
	AllowedBitrates     []int    `json:"allowedBitrates,omitempty"`
	AllowedVideoCodecs  []string `json:"allowedVideoCodecs,omitempty"`
	AllowedAudioCodecs  []string `json:"allowedAudioCodecs,omitempty"`
	StartTimeTicks      int64    `json:"startTimeTicks,omitempty"`
	AudioStreamIndex    *int     `json:"audioStreamIndex,omitempty"`
	SubtitleStreamIndex *int     `json:"subtitleStreamIndex,omitempty"`
	IssuedAt            int64    `json:"iat"`
	NotBefore           int64    `json:"nbf,omitempty"`
	ExpiresAt           int64    `json:"exp"`
	TokenID             string   `json:"jti"`
}

type Locator struct {
	RootID       string `json:"rootId"`
	RelativePath string `json:"relativePath"`
}

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

func Validate(raw string, secret []byte, now time.Time) (Claims, error) {
	var claims Claims
	if len(secret) < 32 {
		return claims, errors.New("token secret must contain at least 32 bytes")
	}
	if len(raw) == 0 || len(raw) > maxTokenSize {
		return claims, errors.New("invalid token size")
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims, errors.New("invalid token format")
	}
	headerBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return claims, errors.New("invalid token header encoding")
	}
	var tokenHeader header
	if err := json.Unmarshal(headerBytes, &tokenHeader); err != nil {
		return claims, errors.New("invalid token header")
	}
	if tokenHeader.Algorithm != "HS256" || tokenHeader.Type != "JWT" {
		return claims, errors.New("unsupported token algorithm or type")
	}

	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return claims, errors.New("invalid token signature encoding")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return claims, errors.New("invalid token signature")
	}

	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return claims, errors.New("invalid token payload encoding")
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, errors.New("invalid token claims")
	}
	if err := validateClaims(claims, now); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func validateClaims(claims Claims, now time.Time) error {
	if claims.Issuer != Issuer || claims.Audience != Audience {
		return errors.New("invalid token issuer or audience")
	}
	if claims.Subject == "" || claims.UserID == "" || claims.MediaItemID == "" ||
		claims.MediaSourceID == "" || claims.PlaySessionID == "" ||
		claims.MediaLocator == "" || claims.TokenID == "" {
		return errors.New("required token claim is missing")
	}
	if claims.Subject != claims.UserID {
		return errors.New("token subject does not match userId")
	}
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt {
		return errors.New("invalid token timestamps")
	}
	issuedAt := time.Unix(claims.IssuedAt, 0)
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if issuedAt.After(now.Add(clockSkew)) {
		return errors.New("token issued in the future")
	}
	if now.Unix() >= claims.ExpiresAt {
		return errors.New("token expired")
	}
	if expiresAt.Sub(issuedAt) > MaxTokenAge {
		return errors.New("token lifetime exceeds maximum")
	}
	if claims.NotBefore > 0 && time.Unix(claims.NotBefore, 0).After(now.Add(clockSkew)) {
		return errors.New("token is not active yet")
	}
	return nil
}

func DecryptLocator(encoded string, secret []byte) (Locator, error) {
	var locator Locator
	combined, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return locator, fmt.Errorf("decode media locator: %w", err)
	}
	key := sha256.Sum256(append([]byte(locatorAAD+"\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return locator, fmt.Errorf("create media locator cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return locator, fmt.Errorf("create media locator authenticator: %w", err)
	}
	if len(combined) < gcm.NonceSize()+gcm.Overhead() {
		return locator, errors.New("encrypted media locator is too short")
	}
	nonce, ciphertext := combined[:gcm.NonceSize()], combined[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(locatorAAD))
	if err != nil {
		return locator, errors.New("cannot authenticate media locator")
	}
	if err := json.Unmarshal(plaintext, &locator); err != nil {
		return locator, errors.New("invalid decrypted media locator")
	}
	if locator.RootID == "" || locator.RelativePath == "" {
		return Locator{}, errors.New("media locator is incomplete")
	}
	return locator, nil
}
