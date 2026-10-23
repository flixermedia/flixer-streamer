package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadSecretFile(t *testing.T) {
	if v, err := ReadSecretFile("", time.Second); v != "" || err != nil {
		t.Fatal("empty path should give nothing")
	}
	file := filepath.Join(t.TempDir(), "secret")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(file, []byte("  value\n"), 0o600)
	}()
	if v, err := ReadSecretFile(file, 5*time.Second); err != nil || v != "value" {
		t.Fatalf("got %q %v", v, err)
	}
	if _, err := ReadSecretFile(filepath.Join(t.TempDir(), "missing"), 10*time.Millisecond); err == nil {
		t.Fatal("expected a timeout error")
	}
}
