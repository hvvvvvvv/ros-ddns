package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func captureDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestDeferredLogsFlushAfterOuterScope(t *testing.T) {
	output := captureDefault(t)
	ctx, flush := WithDeferredLogs(context.Background())
	LogContext(ctx, slog.LevelInfo, "buffered info", "item", "home")
	if output.Len() != 0 {
		t.Fatalf("log emitted before flush: %q", output.String())
	}

	// Nested scopes share the outer buffer and cannot flush it themselves.
	nested, nestedFlush := WithDeferredLogs(ctx)
	LogContext(nested, slog.LevelDebug, "buffered debug")
	nestedFlush()
	if output.Len() != 0 {
		t.Fatalf("nested flush emitted logs: %q", output.String())
	}

	flush()
	text := output.String()
	for _, want := range []string{"buffered info", "buffered debug", "item=home"} {
		if !strings.Contains(text, want) {
			t.Fatalf("flushed log missing %q: %q", want, text)
		}
	}
	if strings.Index(text, "buffered info") > strings.Index(text, "buffered debug") {
		t.Fatalf("deferred logs are out of order: %q", text)
	}
}

func TestLogContextImmediateWithoutScope(t *testing.T) {
	output := captureDefault(t)
	LogContext(context.Background(), slog.LevelWarn, "immediate", "item", "home")
	if !strings.Contains(output.String(), `level=WARN msg=immediate item=home`) {
		t.Fatalf("immediate log = %q", output.String())
	}
}

func TestDeferredLogsCopyArguments(t *testing.T) {
	output := captureDefault(t)
	ctx, flush := WithDeferredLogs(context.Background())
	args := []any{"item", "before"}
	LogContext(ctx, slog.LevelInfo, "copy", args...)
	args[1] = "after"
	flush()
	if !strings.Contains(output.String(), "item=before") || strings.Contains(output.String(), "item=after") {
		t.Fatalf("deferred arguments were not copied: %q", output.String())
	}
}
