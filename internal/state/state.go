// Package state stores the service's recoverable, credential-free cache.
package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const fileVersion = 1

// Item is the last observed source and the independently confirmed targets.
// SourceKey prevents a changed source configuration from reusing a desired value.
type Item struct {
	SourceKey        string            `json:"sourceKey"`
	SourceAddress    string            `json:"sourceAddress,omitempty"`
	RecordValue      string            `json:"recordValue,omitempty"`
	SourceObservedAt time.Time         `json:"sourceObservedAt,omitempty"`
	ValueSince       time.Time         `json:"valueSince,omitempty"`
	DesiredValue     string            `json:"desiredValue,omitempty"`
	DesiredSince     time.Time         `json:"desiredSince,omitempty"`
	LastSyncAt       time.Time         `json:"lastSyncAt,omitempty"`
	Domains          map[string]Domain `json:"domains"`
	AddressList      *AddressList      `json:"addressList,omitempty"`
}

// Domain contains no provider configuration or credentials.
type Domain struct {
	RecordID  string    `json:"recordId,omitempty"`
	Value     string    `json:"value,omitempty"`
	TTL       int       `json:"ttl"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

// AddressList identifies the managed portion of a RouterOS list.
type AddressList struct {
	Family    string    `json:"family"`
	List      string    `json:"list"`
	Comment   string    `json:"comment"`
	Timeout   string    `json:"timeout,omitempty"`
	Value     string    `json:"value,omitempty"`
	Entries   []string  `json:"entries"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

type fileState struct {
	Version int             `json:"version"`
	Items   map[string]Item `json:"items"`
}

// Store serializes updates and writes complete snapshots using a temporary file
// in the destination directory. Its zero value is a usable in-memory store.
type Store struct {
	mu    sync.RWMutex
	path  string
	items map[string]Item
}

// Open reads an existing cache without creating a file. An empty path uses only
// memory. Missing files are normal; corrupt or unsupported files are warned
// about and ignored so that a startup reconciliation can rebuild the cache.
func Open(path string) (*Store, error) {
	s := &Store{path: path, items: make(map[string]Item)}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var disk fileState
	if err = json.Unmarshal(data, &disk); err == nil && (disk.Version != fileVersion || disk.Items == nil) {
		err = fmt.Errorf("unsupported or incomplete state format (version %d)", disk.Version)
	}
	if err != nil {
		slog.Warn("ignoring corrupt state; starting with an empty cache", "path", path, "error", err)
		return s, nil
	}
	s.items = disk.Items
	return s, nil
}

// Get returns an independent copy of an item's cached state.
func (s *Store) Get(name string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[name]
	return cloneItem(it), ok
}

// Put updates memory even if saving fails: state is a cache, and a subsequent
// Put retries persisting the complete current state. Errors remain observable
// to the service. Callers cannot mutate stored slices or maps after Put.
func (s *Store) Put(name string, it Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]Item)
	}
	s.items[name] = cloneItem(it)
	if s.path == "" {
		return nil
	}
	if err := s.save(); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(fileState{Version: fileVersion, Items: s.items}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

func cloneItem(it Item) Item {
	domains := make(map[string]Domain, len(it.Domains))
	for key, d := range it.Domains {
		domains[key] = d
	}
	it.Domains = domains
	if it.AddressList != nil {
		list := *it.AddressList
		list.Entries = append([]string{}, list.Entries...)
		it.AddressList = &list
	}
	return it
}
