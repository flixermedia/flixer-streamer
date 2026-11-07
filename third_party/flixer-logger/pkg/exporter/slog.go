package exporter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/flixermedia/flixer-logger/internal/redact"
)

// Sink is implemented by Exporter and is separated for adapter testing.
type Sink interface {
	Enqueue(Event) error
}

// HandlerOptions describes the producer. OnError must be non-blocking and
// independent of this handler to avoid recursion.
type HandlerOptions struct {
	App        string
	InstanceID string
	Source     string
	OnError    func(error)
}

type handlerOperation struct {
	group string
	attrs []slog.Attr
}

// SlogHandler forwards each enabled record to an existing handler and an
// asynchronous exporter. Existing stdout and Sentry handlers remain intact.
type SlogHandler struct {
	base       slog.Handler
	sink       Sink
	options    HandlerOptions
	operations []handlerOperation
}

func NewSlogHandler(base slog.Handler, sink Sink, options HandlerOptions) (*SlogHandler, error) {
	if base == nil || sink == nil || options.App == "" || options.InstanceID == "" || options.OnError == nil {
		return nil, errors.New("base handler, sink, app, instance ID and error callback are required")
	}
	return &SlogHandler{base: base, sink: sink, options: options}, nil
}

func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *SlogHandler) Handle(ctx context.Context, record slog.Record) error {
	baseErr := h.base.Handle(ctx, record)
	event, err := h.makeEvent(ctx, record)
	if err == nil {
		err = h.sink.Enqueue(event)
	}
	if err != nil {
		h.options.OnError(fmt.Errorf("export log record: %w", err))
	}
	return baseErr
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.base = h.base.WithAttrs(attrs)
	next.operations = append(append([]handlerOperation(nil), h.operations...), handlerOperation{attrs: append([]slog.Attr(nil), attrs...)})
	return &next
}

func (h *SlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.base = h.base.WithGroup(name)
	next.operations = append(append([]handlerOperation(nil), h.operations...), handlerOperation{group: name})
	return &next
}

func (h *SlogHandler) makeEvent(ctx context.Context, record slog.Record) (Event, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Event{}, fmt.Errorf("generate event ID: %w", err)
	}
	var buffer bytes.Buffer
	var jsonHandler slog.Handler = slog.NewJSONHandler(&buffer, nil)
	for _, operation := range h.operations {
		if operation.group != "" {
			jsonHandler = jsonHandler.WithGroup(operation.group)
		} else {
			jsonHandler = jsonHandler.WithAttrs(operation.attrs)
		}
	}
	if err := jsonHandler.Handle(ctx, record); err != nil {
		return Event{}, fmt.Errorf("serialize log attributes: %w", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &fields); err != nil {
		return Event{}, fmt.Errorf("decode log attributes: %w", err)
	}
	delete(fields, slog.TimeKey)
	delete(fields, slog.LevelKey)
	delete(fields, slog.MessageKey)
	fields, err := redact.Fields(fields)
	if err != nil {
		return Event{}, fmt.Errorf("sanitize log attributes: %w", err)
	}
	level := "info"
	switch {
	case record.Level < slog.LevelDebug:
		level = "trace"
	case record.Level < slog.LevelInfo:
		level = "debug"
	case record.Level < slog.LevelWarn:
		level = "info"
	case record.Level < slog.LevelError:
		level = "warn"
	default:
		level = "error"
	}
	message := redact.Text(record.Message)
	if strings.TrimSpace(message) == "" {
		message = "(empty log message)"
	}
	event := Event{
		SchemaVersion: 1, EventID: hex.EncodeToString(id[:]), Time: record.Time.UTC(),
		Level: level, App: redact.Text(h.options.App), InstanceID: redact.Text(h.options.InstanceID),
		Source: redact.Text(h.options.Source), Message: message, Fields: fields,
	}
	event.TraceID, _ = fields["trace_id"].(string)
	event.RequestID, _ = fields["request_id"].(string)
	event.TaskID, _ = fields["task_id"].(string)
	return event, nil
}
