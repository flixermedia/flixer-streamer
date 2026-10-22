package logfile

import (
	"log/slog"
	"strings"
	"time"
)

// UnifiedAttrs reshapes slog's JSON output into the schema shared by every Flixer app:
// time (RFC 3339 UTC), lowercase level, message, plus app/source and flat fields.
func UnifiedAttrs(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Value = slog.StringValue(a.Value.Time().UTC().Format(time.RFC3339Nano))
	case slog.LevelKey:
		a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}
