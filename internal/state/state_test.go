package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoundTripAndIndependentCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Open created a state file: %v", err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	want := Item{
		SourceKey: "source", SourceAddress: "2001:db8::1/64", RecordValue: "2001:db8::1",
		SourceObservedAt: now, DesiredValue: "2001:db8::1", LastSyncAt: now,
		Domains:     map[string]Domain{"binding": {RecordID: "id-1", Value: "2001:db8::1", TTL: 600, UpdatedAt: now, LastError: "retry me"}},
		AddressList: &AddressList{Family: "ipv6", List: "hosts", Comment: "ros-ddns:home", Timeout: "1h", Value: "2001:db8::1", Entries: []string{"2001:db8::1"}, UpdatedAt: now},
	}
	if err := s.Put("home", want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		Version int                                   `json:"version"`
		Items   map[string]map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Version != 1 {
		t.Fatalf("cache version = %d, want 1", legacy.Version)
	}
	// Older version-1 files include stability metadata that is no longer used.
	for _, field := range []string{"valueSince", "desiredSince"} {
		if _, ok := legacy.Items["home"][field]; ok {
			t.Fatalf("persisted unused field %q", field)
		}
		legacy.Items["home"][field] = json.RawMessage(`"2026-10-31T12:00:00Z"`)
	}
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("home")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %#v, want %#v", got, want)
	}
	want.Domains["binding"] = Domain{Value: "mutated"}
	want.AddressList.Entries[0] = "mutated"
	got.Domains["binding"] = Domain{Value: "also mutated"}
	got.AddressList.Entries[0] = "also mutated"
	for _, store := range []*Store{s, reopened} {
		fresh, _ := store.Get("home")
		if fresh.Domains["binding"].Value != "2001:db8::1" || fresh.AddressList.Entries[0] != "2001:db8::1" {
			t.Fatalf("caller mutated stored state: %#v", fresh)
		}
	}
	// Replacing an existing file must work, including on Windows.
	if err := reopened.Put("other", Item{}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"valueSince"`)) || bytes.Contains(data, []byte(`"desiredSince"`)) {
		t.Fatal("rewriting a legacy cache retained unused stability metadata")
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("other"); !ok {
		t.Fatal("replacement did not contain the latest item")
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if len(temps) != 0 {
		t.Fatalf("left temporary files: %v", temps)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions = %v, want 0600", info.Mode().Perm())
	}
}

func TestCorruptStateRecovery(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, data := range []string{"{broken", `{"version":1,"items":{}} trailing`, `{"version":9,"items":{}}`, `null`, `{}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path)
			if err != nil || len(s.items) != 0 {
				t.Fatalf("corrupt cache was not ignored: store=%#v error=%v", s, err)
			}
			if err := s.Put("rebuilt", Item{RecordValue: "192.0.2.1"}); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := reopened.Get("rebuilt"); !ok {
				t.Fatal("recovered cache not persisted")
			}
		})
	}
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "ignoring corrupt state") {
		t.Fatalf("no corruption warning: %s", logs.String())
	}
}

func TestMemoryAndConcurrentUpdates(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			name := fmt.Sprintf("item-%d", i)
			if err := s.Put(name, Item{RecordValue: name}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := range 12 {
		name := fmt.Sprintf("item-%d", i)
		if got, ok := s.Get(name); !ok || got.RecordValue != name {
			t.Fatalf("lost update for %s: %#v", name, got)
		}
	}
	var zero Store
	if err := zero.Put("zero", Item{}); err != nil {
		t.Fatal(err)
	}
}

func TestReadAndWriteErrorsAreVisible(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Fatal("reading a directory should fail")
	}
	path := filepath.Join(dir, "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("kept-in-memory", Item{RecordValue: "192.0.2.1"}); err == nil {
		t.Fatal("replacing a directory should fail")
	}
	if _, ok := s.Get("kept-in-memory"); !ok {
		t.Fatal("write failure lost the in-memory cache")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].Name() != "state.json" {
		t.Fatalf("failed write left temporary files: %v", files)
	}
}
