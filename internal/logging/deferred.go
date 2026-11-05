package logging

import (
	"context"
	"log/slog"
	"sync"
)

type deferredLogsKey struct{}

type deferredLog struct {
	ctx     context.Context
	level   slog.Level
	message string
	args    []any
}

type deferredLogs struct {
	mu      sync.Mutex
	entries []deferredLog
}

// WithDeferredLogs collects logs across a call chain without invoking slog while
// caller-owned locks are held. Call it before taking those locks and invoke the
// returned flush only after releasing all of them. Nested calls share the outer
// buffer; the outer caller remains responsible for flushing it.
func WithDeferredLogs(ctx context.Context) (context.Context, func()) {
	if _, ok := ctx.Value(deferredLogsKey{}).(*deferredLogs); ok {
		return ctx, func() {}
	}
	logs := &deferredLogs{}
	return context.WithValue(ctx, deferredLogsKey{}, logs), func() {
		logs.mu.Lock()
		entries := logs.entries
		logs.entries = nil
		logs.mu.Unlock()
		for _, entry := range entries {
			emitLog(entry.ctx, entry.level, entry.message, entry.args...)
		}
	}
}

// LogContext defers output when ctx carries a deferred buffer; otherwise it logs
// immediately. Arguments must be immutable snapshots, never pointers to mutable
// state owned by a caller holding a lock.
func LogContext(ctx context.Context, level slog.Level, message string, args ...any) {
	if logs, ok := ctx.Value(deferredLogsKey{}).(*deferredLogs); ok {
		logs.mu.Lock()
		logs.entries = append(logs.entries, deferredLog{ctx, level, message, append([]any(nil), args...)})
		logs.mu.Unlock()
		return
	}
	emitLog(ctx, level, message, args...)
}

func emitLog(ctx context.Context, level slog.Level, message string, args ...any) {
	switch level {
	case slog.LevelDebug:
		slog.DebugContext(ctx, message, args...)
	case slog.LevelInfo:
		slog.InfoContext(ctx, message, args...)
	case slog.LevelWarn:
		slog.WarnContext(ctx, message, args...)
	case slog.LevelError:
		slog.ErrorContext(ctx, message, args...)
	}
}
