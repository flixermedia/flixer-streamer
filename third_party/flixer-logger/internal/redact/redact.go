// Package redact removes common sensitive values from untrusted log fields.
// Producers must also avoid emitting sensitive content; pattern matching cannot
// identify every secret or piece of personal data.
package redact

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const replacement = "[REDACTED]"

var (
	sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|cookie|api[_-]?key|private[_-]?key|credential|prompt|embedding|request[_-]?body)`)
	bearer       = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9._~+/-]+=*`)
	cookies      = regexp.MustCompile(`(?i)\b(set-cookie|cookie)["']?\s*[:=]\s*[^\r\n]*`)
	assignments  = regexp.MustCompile(`(?i)\b[a-z0-9_-]*(password|passwd|secret|token|authorization|api[_-]?key|credential|private[_-]?key)[a-z0-9_-]*["']?\s*[:=]\s*("(\\.|[^"\\])*"|'(\\.|[^'\\])*'|[^\s,;}]+)`)
	url          = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>]+`)
	email        = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)
	path         = regexp.MustCompile(`(?:^|[\s("'=])(?:/[A-Za-z0-9._-]+){2,}[^\s"']*`)
)

// Text masks common credential values, network addresses and file paths.
func Text(value string) string {
	if strings.ContainsAny(value, " \t\n\f\r") {
		value = bearer.ReplaceAllString(value, replacement)
	}
	if strings.ContainsAny(value, ":=") {
		value = cookies.ReplaceAllString(value, replacement)
		value = assignments.ReplaceAllString(value, replacement)
	}
	if strings.Contains(value, "://") {
		value = url.ReplaceAllString(value, replacement)
	}
	if strings.Contains(value, "@") {
		value = email.ReplaceAllString(value, replacement)
	}
	if strings.Contains(value, "/") {
		value = path.ReplaceAllStringFunc(value, func(match string) string {
			if strings.HasPrefix(match, "/") {
				return replacement
			}
			return match[:1] + replacement
		})
	}
	return value
}

// Fields returns a sanitized copy of JSON-compatible structured fields.
// Excessive nesting or unsupported types are rejected rather than logged.
func Fields(fields map[string]any) (map[string]any, error) {
	count := 0
	value, err := visit(fields, 0, &count)
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}

func visit(value any, depth int, count *int) (any, error) {
	*count++
	if depth > 8 || *count > 10000 {
		return nil, errors.New("log fields exceed nesting or node limit")
	}
	switch v := value.(type) {
	case nil, bool, float64:
		return v, nil
	case string:
		if len(v) > 16384 {
			return nil, errors.New("log field exceeds string size limit")
		}
		return Text(v), nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			var err error
			out[i], err = visit(item, depth+1, count)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if len(key) > 256 {
				return nil, errors.New("log field key exceeds size limit")
			}
			if sensitiveKey.MatchString(key) {
				out[key] = replacement
				continue
			}
			clean, err := visit(item, depth+1, count)
			if err != nil {
				return nil, err
			}
			out[key] = clean
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported log field type %T", value)
	}
}
