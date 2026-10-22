package httpserver

import "testing"

func TestToWebVTT(t *testing.T) {
	got := toWebVTT([]byte("\ufeff1\r\n00:00:01,500 --> 00:00:03,000\r\nHello, world\r\n"))
	want := "WEBVTT\n\n1\n00:00:01.500 --> 00:00:03.000\nHello, world\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if out := toWebVTT([]byte("WEBVTT\n\n00:01.000 --> 00:02.000\nHi")); out != "WEBVTT\n\n00:01.000 --> 00:02.000\nHi" {
		t.Fatalf("vtt altered: %q", out)
	}
}
