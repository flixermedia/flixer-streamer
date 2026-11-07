package redact

import (
	"strings"
	"testing"
)

func TestTextMasksSensitiveValues(t *testing.T) {
	input := `failed Bearer abc.xyz password="my pass" https://private.example/path alice@example.com /srv/media/secret`
	got := Text(input)
	for _, secret := range []string{"abc.xyz", "my pass", "private.example", "alice@example.com", "/srv/media/secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("value %q was not masked: %q", secret, got)
		}
	}
}

func TestTextMasksQuotedKeysAndCookieHeaders(t *testing.T) {
	input := "{\"access_token\":\"jsonsecret\", \"private_key\":\"top secret\"}\nCookie: session=private; other=private2"
	got := Text(input)
	for _, secret := range []string{"jsonsecret", "top secret", "session=private", "private2"} {
		if strings.Contains(got, secret) {
			t.Fatalf("value %q was not masked: %q", secret, got)
		}
	}
}

func TestFieldsRecursesWithoutMutatingInput(t *testing.T) {
	input := map[string]any{
		"api_key": "do-not-store",
		"nested":  []any{map[string]any{"password": "private", "message": "see https://example.com"}},
		"status":  float64(200),
	}
	output, err := Fields(input)
	if err != nil {
		t.Fatal(err)
	}
	if output["api_key"] != replacement || input["api_key"] != "do-not-store" {
		t.Fatal("key was not redacted or input was modified")
	}
	nested := output["nested"].([]any)[0].(map[string]any)
	if nested["password"] != replacement || strings.Contains(nested["message"].(string), "example.com") {
		t.Fatalf("nested data not sanitized: %#v", nested)
	}
}

func TestFieldsRejectsInvalidOrExcessiveData(t *testing.T) {
	if _, err := Fields(map[string]any{"value": make(chan int)}); err == nil {
		t.Fatal("unsupported value accepted")
	}

	if _, err := Fields(map[string]any{"value": strings.Repeat("x", 16385)}); err == nil {
		t.Fatal("oversized value accepted")
	}
	var nested any = "value"
	for range 10 {
		nested = []any{nested}
	}
	if _, err := Fields(map[string]any{"nested": nested}); err == nil {
		t.Fatal("excessive nesting accepted")
	}
}

func FuzzTextMatchesOriginal(f *testing.F) {
	for _, input := range []string{
		"normal playback completed", "Bearer value", "TOKEN='some secret'", "https://example.com/a",
		"Cookie: value=private", "name@example.com", "/srv/media/file", "ä Bearer\tabc= /tmp/one",
		`{"authorization":"Basic abc=","nested":"https://example.com/path?token=secret"}`,
	} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, value string) {
		want := bearer.ReplaceAllString(value, replacement)
		want = cookies.ReplaceAllString(want, replacement)
		want = assignments.ReplaceAllString(want, replacement)
		want = url.ReplaceAllString(want, replacement)
		want = email.ReplaceAllString(want, replacement)
		want = path.ReplaceAllStringFunc(want, func(match string) string {
			if strings.HasPrefix(match, "/") {
				return replacement
			}
			return match[:1] + replacement
		})
		if got := Text(value); got != want {
			t.Fatalf("redaction changed for %q: got %q, want %q", value, got, want)
		}
	})
}
