package exporter

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRetryDelay = 30 * time.Second

func retryBackoff(base time.Duration, attempt int) time.Duration {
	delay := min(base, maxRetryDelay)
	for i := 0; i < attempt && delay < maxRetryDelay; i++ {
		if delay > maxRetryDelay/2 {
			delay = maxRetryDelay
		} else {
			delay *= 2
		}
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds >= uint64(maxRetryDelay/time.Second) {
			return maxRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return min(when.Sub(now), maxRetryDelay)
}
