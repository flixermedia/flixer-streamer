package config

import (
	"path/filepath"
	"testing"
)

func TestParseMediaRootsJSON(t *testing.T) {
	roots, err := parseMediaRoots(`["C:\\Media\\Movies","D:\\Media\\TV"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0].ID != "root-1" || roots[1].ID != "root-2" {
		t.Fatalf("roots = %#v, want two ordered roots", roots)
	}
}

func TestParseMediaRootsPlatformList(t *testing.T) {
	separator := string(filepath.ListSeparator)
	roots, err := parseMediaRoots("one" + separator + "two")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0].Path != "one" || roots[1].Path != "two" {
		t.Fatalf("roots = %#v, want [one two]", roots)
	}
}

func TestParseMediaRootsRejectsInvalidValues(t *testing.T) {
	values := []string{"", `["unterminated]`}
	if filepath.ListSeparator == ':' {
		values = append(values, "one::two")
	}
	for _, value := range values {
		if roots, err := parseMediaRoots(value); err == nil {
			t.Errorf("parseMediaRoots(%q) = %#v, nil error", value, roots)
		}
	}
}
