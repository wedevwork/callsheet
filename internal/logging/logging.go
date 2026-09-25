// Package logging builds the structured process logger shared by services.
//
// Records are JSON with a UTC timestamp, level and message. Callers attach a
// component and optional node/role/task/request ids; nothing else (prompts,
// manuals, environment, keys, payloads) is logged implicitly.
package logging

import (
	"io"
	"log/slog"
)

// Standard attribute keys.
const (
	KeyComponent = "component"
	KeyNode      = "node_id"
	KeyRole      = "role"
	KeyTask      = "task_id"
	KeyRequest   = "request_id"
)

// New returns a JSON slog.Logger writing to w at the given minimum level.
// Timestamps are rendered in UTC.
func New(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				return slog.Time(slog.TimeKey, a.Value.Time().UTC())
			}
			return a
		},
	})
	return slog.New(h)
}

// Component returns l annotated with the component name.
func Component(l *slog.Logger, name string) *slog.Logger {
	return l.With(slog.String(KeyComponent, name))
}
