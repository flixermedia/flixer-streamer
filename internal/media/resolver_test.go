package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolverOpensFileFromConfiguredRoots(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	filename := filepath.Join(secondRoot, "movie.mp4")
	if err := os.WriteFile(filename, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver([]Root{
		{ID: "movies", Path: firstRoot},
		{ID: "tv", Path: secondRoot},
	})
	if err != nil {
		t.Fatal(err)
	}

	file, err := resolver.Open("tv", "movie.mp4")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "movie.mp4" {
		t.Fatalf("opened %q, want movie.mp4", info.Name())
	}
}

func TestResolverRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.mp4"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver([]Root{{ID: "movies", Path: root}})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../secret.mp4", "/secret.mp4", `..\secret.mp4`, "C:/secret.mp4"} {
		if file, err := resolver.Open("movies", name); file != nil || !errors.Is(err, ErrInvalidLocator) {
			if file != nil {
				_ = file.Close()
			}
			t.Errorf("Open(%q) = (%v, %v), want ErrInvalidLocator", name, file, err)
		}
	}

	link := filepath.Join(root, "external")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if file, err := resolver.Open("movies", "external/secret.mp4"); file != nil || !errors.Is(err, ErrInvalidLocator) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("symlink escape Open() = (%v, %v), want ErrInvalidLocator", file, err)
	}
}
