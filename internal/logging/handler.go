package logging

import (
	"context"
	"log/slog"
	"os"

	"ros-ddns/internal/redact"
)

type handler struct {
	base  slog.Handler
	hub   *Hub
	level *slog.LevelVar
	steps []handlerStep
}

type handlerStep struct {
	group string
	attrs []slog.Attr // resolved, owned values; redacted at Handle time
}

var _ slog.Handler = (*handler)(nil)

// NewHandler sends sanitized records to base and hub. Both base.Enabled and an
// optional LevelVar must permit the level. A nil base uses a stderr text handler;
// a nil hub permits stderr-only use. Configure base with the same LevelVar when
// both thresholds should change together.
//
// Base receives normal slog groups; hub attributes use dot-separated group
// prefixes. Duplicate flattened keys use the last value. Values are copied into
// JSON-safe data, including nested string/error values, before either output.
func NewHandler(base slog.Handler, hub *Hub, level *slog.LevelVar) slog.Handler {
	if base == nil {
		opts := &slog.HandlerOptions{}
		if level != nil {
			opts.Level = level
		}
		base = slog.NewTextHandler(os.Stderr, opts)
	}
	return &handler{base: base, hub: hub, level: level}
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return (h.level == nil || level >= h.level.Level()) && h.base.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, record slog.Record) error {
	if !h.Enabled(ctx, record.Level) {
		return nil
	}
	var r *redact.Redactor
	if h.hub != nil {
		r = h.hub.redactor.Load()
	}
	base := h.base
	attrs := make(map[string]any)
	prefix := ""
	for _, step := range h.steps {
		if step.group != "" {
			group := r.Replace(step.group)
			base = base.WithGroup(group)
			prefix += group + "."
		} else {
			bound := redactAttrs(step.attrs, r)
			flattenAttrs(attrs, prefix, bound)
			base = base.WithAttrs(bound)
		}
	}
	raw := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(a slog.Attr) bool {
		raw = append(raw, a)
		return true
	})
	values := redactAttrs(newNormalizer().attrs(raw, 0), r)
	flattenAttrs(attrs, prefix, values)
	message := r.Replace(record.Message)
	clean := slog.NewRecord(record.Time, record.Level, message, record.PC)
	clean.AddAttrs(values...)
	if h.hub != nil {
		h.hub.publish(Entry{Time: record.Time, Message: message, Attrs: attrs}, record.Level)
	}
	// Publication is independent of base I/O errors.
	return base.Handle(ctx, clean)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	values := newNormalizer().attrs(attrs, 0)
	if len(values) == 0 {
		return h
	}
	return h.withStep(handlerStep{attrs: values})
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.withStep(handlerStep{group: name})
}

func (h *handler) withStep(step handlerStep) *handler {
	next := *h
	next.steps = make([]handlerStep, len(h.steps), len(h.steps)+1)
	copy(next.steps, h.steps)
	next.steps = append(next.steps, step)
	return &next
}

func redactAttrs(attrs []slog.Attr, r *redact.Redactor) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		key := r.Replace(a.Key)
		if a.Value.Kind() == slog.KindGroup {
			out[i] = slog.Attr{Key: key, Value: slog.GroupValue(redactAttrs(a.Value.Group(), r)...)}
		} else {
			out[i] = slog.Any(key, copyValue(a.Value.Any(), r))
		}
	}
	return out
}

func flattenAttrs(out map[string]any, prefix string, attrs []slog.Attr) {
	for _, a := range attrs {
		if a.Value.Kind() == slog.KindGroup {
			group := prefix
			if a.Key != "" {
				group += a.Key + "."
			}
			flattenAttrs(out, group, a.Value.Group())
		} else {
			out[prefix+a.Key] = copyValue(a.Value.Any(), nil)
		}
	}
}

// copyValue only receives normalized, acyclic JSON values. Each output owns its
// maps/slices so a base handler, subscriber or History caller cannot mutate peers.
func copyValue(value any, r *redact.Redactor) any {
	switch v := value.(type) {
	case string:
		return r.Replace(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[r.Replace(key)] = copyValue(value, r)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = copyValue(value, r)
		}
		return out
	default:
		return v
	}
}
