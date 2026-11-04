package transcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
)

// HLSKey derives the AES-128 key for one stream token. It is only handed out by the authenticated key endpoint, so a
// saved playlist and its segments are useless without a live token.
func HLSKey(secret []byte, tokenID, mediaItemID string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("flixer-streamer:hls-key:v1\x00" + tokenID + "\x00" + mediaItemID))
	return mac.Sum(nil)[:16]
}

// HLSIV is the IV HLS players assume when the playlist gives none: the segment's media sequence number, big endian.
func HLSIV(segment int) []byte {
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], uint64(segment))
	return iv
}

// EncryptingWriter encrypts a stream as AES-128-CBC with PKCS#7 padding. Close must be called once the source ends cleanly.
type EncryptingWriter struct {
	out     io.Writer
	mode    cipher.BlockMode
	pending []byte
}

func NewEncryptingWriter(out io.Writer, key, iv []byte) (*EncryptingWriter, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &EncryptingWriter{out: out, mode: cipher.NewCBCEncrypter(block, iv)}, nil
}

func (w *EncryptingWriter) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	whole := len(w.pending) - len(w.pending)%aes.BlockSize
	if whole > 0 {
		chunk := make([]byte, whole)
		w.mode.CryptBlocks(chunk, w.pending[:whole])
		if _, err := w.out.Write(chunk); err != nil {
			return 0, err
		}
		w.pending = append(w.pending[:0], w.pending[whole:]...)
	}
	return len(data), nil
}

func (w *EncryptingWriter) Close() error {
	padding := aes.BlockSize - len(w.pending)
	last := append(w.pending, make([]byte, padding)...)
	for i := len(w.pending); i < len(last); i++ {
		last[i] = byte(padding)
	}
	w.mode.CryptBlocks(last, last)
	_, err := w.out.Write(last)
	return err
}
