package transcode

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestEncryptingWriterRoundTrip(t *testing.T) {
	key := HLSKey([]byte("secret"), "session", "item")
	iv := HLSIV(7)
	plain := bytes.Repeat([]byte("segment-bytes"), 1000)
	var out bytes.Buffer
	w, err := NewEncryptingWriter(&out, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(plain); i += 333 {
		end := min(i+333, len(plain))
		if _, err := w.Write(plain[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	data := out.Bytes()
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	data = data[:len(data)-int(data[len(data)-1])]
	if !bytes.Equal(data, plain) {
		t.Fatal("decrypted segment does not match")
	}
}
