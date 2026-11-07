package exporter

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryBackoffBounds(t *testing.T) {
	for _, tc := range []struct {
		base    time.Duration
		attempt int
		max     time.Duration
	}{
		{time.Second, 0, time.Second},
		{time.Second, 3, 8 * time.Second},
		{time.Second, 19, maxRetryDelay},
		{time.Hour, 1, maxRetryDelay},
	} {
		for i := 0; i < 100; i++ {
			delay := retryBackoff(tc.base, tc.attempt)
			if delay < tc.max/2 || delay > tc.max {
				t.Fatalf("unexpected jitter delay %v for %+v", delay, tc)
			}
		}
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"1", time.Second},
		{"100000000000", maxRetryDelay},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(time.Hour).Format(http.TimeFormat), maxRetryDelay},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"-1", 0},
		{"garbage", 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Fatalf("retry after %q: got %v want %v", tc.value, got, tc.want)
		}
	}
}
