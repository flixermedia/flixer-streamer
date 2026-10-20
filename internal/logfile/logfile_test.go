package logfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "streamer.log")
	w, err := Open(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for _, line := range []string{"aaaaaa\n", "bbbbbb\n", "cccccc\n", "dddddd\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]string{path: "dddddd\n", path + ".1": "cccccc\n", path + ".2": "bbbbbb\n"}
	for name, content := range want {
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("%s = %q, want %q", filepath.Base(name), got, content)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("expected no third backup, got err=%v", err)
	}
}
