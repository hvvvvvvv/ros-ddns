package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/slogtest"
	"time"

	"ros-ddns/internal/redact"
)

func TestHandlerAttrsGroupsAndLevels(t *testing.T) {
	var output bytes.Buffer
	var level slog.LevelVar
	hub := NewHub(10)
	defer hub.Close()
	h := NewHandler(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}), hub, &level)
	root := slog.New(h).With("root", "root-value")
	grouped := root.WithGroup("request").With("id", 7).WithGroup("details")
	grouped.Debug("filtered")
	grouped.Info("logged", "err", errors.New("failure"), "ok", true,
		slog.Group("nested", "value", 9), slog.Group("", "inline", "yes"), slog.Group("empty"), slog.Attr{})
	root.Info("root only", "key", "value")
	level.Set(slog.LevelDebug)
	grouped.Debug("now enabled", "number", 1.5)
	level.Set(slog.LevelError)
	grouped.Warn("filtered warning")
	grouped.Error("error")
	entries, cursor := hub.History(0, 10, slog.LevelDebug)
	if len(entries) != 4 || cursor != 4 {
		t.Fatalf("entries = %#v, cursor = %d", entries, cursor)
	}
	want := map[string]any{
		"root": "root-value", "request.id": int64(7), "request.details.err": "failure",
		"request.details.ok": true, "request.details.nested.value": int64(9), "request.details.inline": "yes",
	}
	if !reflect.DeepEqual(entries[0].Attrs, want) {
		t.Fatalf("attrs = %#v, want %#v", entries[0].Attrs, want)
	}
	if !reflect.DeepEqual(entries[1].Attrs, map[string]any{"root": "root-value", "key": "value"}) {
		t.Fatalf("child mutated root handler: %#v", entries[1])
	}
	for i, want := range []string{"info", "info", "debug", "error"} {
		if entries[i].Level != want || entries[i].Time.IsZero() {
			t.Fatalf("entry %d = %#v", i, entries[i])
		}
	}
	logs := decodeLogs(t, output.Bytes())
	if len(logs) != 4 {
		t.Fatalf("base received %d records", len(logs))
	}
	request := logs[0]["request"].(map[string]any)
	if request["id"] != float64(7) || request["details"].(map[string]any)["err"] != "failure" {
		t.Fatalf("base lost normal slog groups: %#v", logs[0])
	}
	baseFiltered := NewHub(1)
	defer baseFiltered.Close()
	logger := slog.New(NewHandler(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}), baseFiltered, nil))
	logger.Warn("base rejects this")
	logger.Error("base accepts this")
	if got, _ := baseFiltered.History(0, 10, slog.LevelDebug); len(got) != 1 || got[0].Message != "base accepts this" {
		t.Fatalf("base.Enabled was ignored: %#v", got)
	}
}

func TestHandlerSlogContract(t *testing.T) {
	var output bytes.Buffer
	hub := NewHub(100)
	defer hub.Close()
	h := NewHandler(slog.NewJSONHandler(&output, nil), hub, nil)
	if err := slogtest.TestHandler(h, func() []map[string]any { return decodeLogs(t, output.Bytes()) }); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerRedactsBothOutputsAndBoundAttrs(t *testing.T) {
	const secret = "credential\"with\\slash\nand newline<&>"
	encoded, _ := json.Marshal(secret)
	variants := []string{secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret)}
	var output bytes.Buffer
	hub := NewHub(20)
	defer hub.Close()
	logger := slog.New(NewHandler(slog.NewJSONHandler(&output, nil), hub, nil)).With("bound", secret)
	// WithAttrs precedes the update: preformatted base attributes must not leak.
	hub.SetRedactor(redact.New([]string{secret, "p", "<&>", "redacted"}))
	for _, variant := range variants {
		logger.Info("credential="+variant, "credential", variant, "err", errors.New(variant),
			"nested", map[string]any{secret: []any{variant, errors.New(variant)}},
			"bytes", []byte(secret), slog.Group("group", "value", variant), slog.String(secret, variant))
	}
	logger.Info("p poll [redacted] redacted", "short", "p poll", "escapedShort", `x%3C%26%3Ey %3C%26%3E x\u003c\u0026\u003ey \u003c\u0026\u003e`)
	entries, _ := hub.History(0, 20, slog.LevelDebug)
	logs := decodeLogs(t, output.Bytes())
	if len(entries) != 4 || len(logs) != 4 {
		t.Fatalf("entry counts = %d/%d", len(entries), len(logs))
	}
	for i := range variants {
		if entries[i].Message != "credential=[redacted]" || logs[i]["msg"] != "credential=[redacted]" {
			t.Fatalf("message leaked: %#v / %#v", entries[i], logs[i])
		}
		for _, key := range []string{"bound", "credential", "err", "bytes", "[redacted]"} {
			if entries[i].Attrs[key] != "[redacted]" || logs[i][key] != "[redacted]" {
				t.Fatalf("%s leaked: %#v / %#v", key, entries[i], logs[i])
			}
		}
		if entries[i].Attrs["group.value"] != "[redacted]" || logs[i]["group"].(map[string]any)["value"] != "[redacted]" {
			t.Fatal("group value leaked")
		}
		for _, attrs := range []map[string]any{entries[i].Attrs, logs[i]} {
			if got := attrs["nested"].(map[string]any)["[redacted]"]; !reflect.DeepEqual(got, []any{"[redacted]", "[redacted]"}) {
				t.Fatalf("nested value leaked: %#v", got)
			}
		}
	}
	if entries[3].Message != "[redacted] poll [redacted] [redacted]" || entries[3].Attrs["short"] != "[redacted] poll" {
		t.Fatalf("short boundaries/markers changed: %#v", entries[3])
	}
	const shortWant = `x%3C%26%3Ey [redacted] x\u003c\u0026\u003ey [redacted]`
	if entries[3].Attrs["escapedShort"] != shortWant || logs[3]["escapedShort"] != shortWant {
		t.Fatalf("escaped short-secret boundaries changed: %#v", entries[3])
	}
	// Existing derived handlers must pick up a replacement redactor as well.
	bound := logger.With("new", "replacement-secret")
	hub.SetRedactor(redact.New([]string{secret, "replacement-secret"}))
	bound.Info("replacement-secret")
	last, _ := hub.History(4, 1, slog.LevelInfo)
	if len(last) != 1 || last[0].Message != "[redacted]" || last[0].Attrs["new"] != "[redacted]" {
		t.Fatalf("redactor update ignored: %#v", last)
	}
	if strings.Contains(output.String(), "replacement-secret") {
		t.Fatal("base ignored updated redactor")
	}
}

type panicError struct{}

func (panicError) Error() string { panic("error-method-secret") }

type panicMarshaler struct {
	Value string `json:"value"`
}

func (panicMarshaler) MarshalJSON() ([]byte, error) { panic("must not call marshaler") }
func (panicMarshaler) String() string               { panic("must not call String") }

type countingValue struct{ calls *atomic.Int32 }

func (v countingValue) LogValue() slog.Value {
	v.calls.Add(1)
	return slog.GroupValue(slog.String("resolved", "value-secret"))
}

type recursiveValue struct{}

func (recursiveValue) LogValue() slog.Value { return slog.AnyValue(recursiveValue{}) }

type panicValue struct{}

func (panicValue) LogValue() slog.Value { panic("value-secret") }

func TestHandlerArbitraryValuesAreSafeAndOwned(t *testing.T) {
	var output bytes.Buffer
	hub := NewHub(10)
	defer hub.Close()
	hub.SetRedactor(redact.New([]string{"value-secret"}))
	var calls atomic.Int32
	boundMap := map[string]any{"slice": []any{"value-secret", int64(1)}}
	logger := slog.New(NewHandler(slog.NewJSONHandler(&output, nil), hub, nil)).With("bound", boundMap, "valuer", countingValue{&calls})
	boundMap["slice"].([]any)[0] = "mutated"
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	slice := []any{nil}
	slice[0] = slice
	type node struct {
		Secret string `json:"secret"`
		Next   *node  `json:"next"`
		Ignore string `json:"-"`
	}
	ptr := &node{Secret: "value-secret", Ignore: "value-secret"}
	ptr.Next = ptr
	var nilErr *panicError
	logger.Info("safe", "map", cyclic, "slice", slice, "ptr", ptr, "nan", math.NaN(), "inf", math.Inf(1),
		"func", func() {}, "chan", make(chan int), "complex", complex(1, 2), "nilErr", nilErr,
		"error", panicError{}, "marshaler", panicMarshaler{"value-secret"}, "recurse", recursiveValue{},
		"panic", panicValue{}, "nestedValue", map[string]any{"valuer": countingValue{&calls}},
		"time", time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), "duration", time.Second,
		"keys", map[int]string{1: "value-secret"})
	logger.Info("again")
	if calls.Load() != 2 {
		t.Fatalf("LogValue called %d times, want once per bound/record value", calls.Load())
	}
	entries, _ := hub.History(0, 10, slog.LevelDebug)
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	attrs := entries[0].Attrs
	if attrs["map"].(map[string]any)["self"] != "[cycle]" || attrs["slice"].([]any)[0] != "[cycle]" || attrs["ptr"].(map[string]any)["next"] != "[cycle]" {
		t.Fatalf("cycles not detected: %#v", attrs)
	}
	if attrs["bound"].(map[string]any)["slice"].([]any)[0] != "[redacted]" || attrs["valuer.resolved"] != "[redacted]" {
		t.Fatalf("bound values not snapshotted/resolved: %#v", attrs)
	}
	if attrs["error"] != unavailable || attrs["nilErr"] != nil || attrs["nan"] != "NaN" || attrs["inf"] != "+Inf" {
		t.Fatalf("unsafe scalar values: %#v", attrs)
	}
	if attrs["marshaler"].(map[string]any)["value"] != "[redacted]" || attrs["ptr"].(map[string]any)["secret"] != "[redacted]" {
		t.Fatalf("nested secret leaked: %#v", attrs)
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("hub values are not JSON-safe: %v", err)
	}
	if strings.Contains(string(data), "value-secret") || strings.Contains(output.String(), "value-secret") || strings.Contains(output.String(), "error-method-secret") {
		t.Fatal("arbitrary values leaked raw secrets or panic contents")
	}
	if len(decodeLogs(t, output.Bytes())) != 2 {
		t.Fatal("base output is not valid JSON")
	}
}

func TestHandlerLimitsRecursiveAndLargeValues(t *testing.T) {
	hub := NewHub(2)
	defer hub.Close()
	logger := slog.New(NewHandler(slog.NewJSONHandler(io.Discard, nil), hub, nil))
	var deep any = "leaf"
	for range 100 {
		deep = []any{deep}
	}
	large := make([]int, maxValueNodes+1)
	logger.Info("depth", "value", deep)
	logger.Info("size", "value", large)
	entries, _ := hub.History(0, 10, slog.LevelInfo)
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil || !bytes.Contains(data, []byte(truncated)) {
			t.Fatalf("unbounded value: %s, %v", data, err)
		}
	}
}

type failingHandler struct{ slog.Handler }

func (f failingHandler) Handle(context.Context, slog.Record) error { return io.ErrClosedPipe }

func TestHandlerPublishesDespiteBaseErrorAndKeepsRecordMetadata(t *testing.T) {
	hub := NewHub(2)
	defer hub.Close()
	h := NewHandler(failingHandler{slog.NewTextHandler(io.Discard, nil)}, hub, nil)
	when := time.Date(2026, 10, 31, 0, 0, 0, 123, time.UTC)
	r := slog.NewRecord(when, slog.LevelWarn, "record", 0)
	r.AddAttrs(slog.String("key", "value"))
	if err := h.Handle(context.Background(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("base error = %v", err)
	}
	entries, _ := hub.History(0, 1, slog.LevelDebug)
	if len(entries) != 1 || entries[0].Time != when || entries[0].Level != "warn" {
		t.Fatalf("lost record despite base failure: %#v", entries)
	}
}

func decodeLogs(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var logs []map[string]any
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err == io.EOF {
			return logs
		} else if err != nil {
			t.Fatalf("invalid base JSON: %v\n%s", err, data)
		}
		logs = append(logs, entry)
	}
}
