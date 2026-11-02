// Package logging provides a bounded, in-memory log stream for the WebUI.
package logging

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"ros-ddns/internal/redact"
)

type Entry struct {
	Seq     uint64         `json:"seq"`
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

type storedEntry struct {
	entry Entry
	level slog.Level
}

// Hub owns its retained entries and gives each reader independent attribute
// maps and slices. It starts no goroutines. Create a Hub with NewHub.
type Hub struct {
	mu          sync.Mutex
	ring        []storedEntry
	start       int
	size        int
	seq         uint64
	subscribers map[chan Entry]uint64
	closed      bool
	redactor    atomic.Pointer[redact.Redactor]
}

func NewHub(capacity int) *Hub {
	if capacity <= 0 {
		capacity = 2000
	}
	return &Hub{ring: make([]storedEntry, capacity), subscribers: make(map[chan Entry]uint64)}
}

// SetRedactor atomically selects the redactor for subsequent log records,
// including attributes bound before this call. Nil disables redaction.
// Already retained entries are not rewritten.
func (h *Hub) SetRedactor(r *redact.Redactor) {
	h.redactor.Store(r)
}

// History returns retained entries in sequence order, filtered by level.
// Nonpositive limits default to 200; limits above 1000 are capped at 1000.
// The second result is a pagination cursor: the last sequence examined, including
// filtered entries. Passing it as after never skips an entry because of a limit.
// A stale cursor starts at the oldest retained entry; a future cursor is unchanged.
func (h *Hub) History(after uint64, limit int, minLevel slog.Level) ([]Entry, uint64) {
	if limit <= 0 {
		limit = 200
	} else if limit > 1000 {
		limit = 1000
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Entry, 0, min(limit, h.size))
	cursor := after
	for i := 0; i < h.size; i++ {
		stored := h.ring[(h.start+i)%len(h.ring)]
		if stored.entry.Seq <= after {
			continue
		}
		cursor = stored.entry.Seq
		if stored.level >= minLevel {
			out = append(out, cloneEntry(stored.entry))
			if len(out) == limit {
				break
			}
		}
	}
	return out, cursor
}

// Subscribe atomically queues retained entries newer than after, then receives
// live entries newer than after. Its buffer has the ring's capacity. Publishing
// never waits for a reader: on overflow, the oldest queued entry is dropped.
// Sequence gaps therefore indicate loss. Cancel is idempotent and closes the
// channel, whose already buffered entries may still be read. After Close,
// Subscribe returns an empty, closed channel.
func (h *Hub) Subscribe(after uint64) (<-chan Entry, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan Entry, len(h.ring))
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	for i := 0; i < h.size; i++ {
		e := h.ring[(h.start+i)%len(h.ring)].entry
		if e.Seq > after {
			ch <- cloneEntry(e)
		}
	}
	h.subscribers[ch] = after
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}

// Close stops publication and closes every subscription. History stays readable.
// Repeated calls are harmless.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subscribers {
		close(ch)
		delete(h.subscribers, ch)
	}
}

func (h *Hub) publish(e Entry, level slog.Level) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.seq++
	e.Seq = h.seq
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Level = levelName(level)
	e = cloneEntry(e)
	if h.size == len(h.ring) {
		h.ring[h.start] = storedEntry{e, level}
		h.start = (h.start + 1) % len(h.ring)
	} else {
		h.ring[(h.start+h.size)%len(h.ring)] = storedEntry{e, level}
		h.size++
	}
	for ch, after := range h.subscribers {
		if e.Seq <= after {
			continue
		}
		entry := cloneEntry(e)
		select {
		case ch <- entry:
		default:
			select {
			case <-ch:
			default:
			}
			// Only this lock's owner sends; a receiver can only free space.
			ch <- entry
		}
	}
}

func cloneEntry(e Entry) Entry {
	if e.Attrs != nil {
		e.Attrs = copyValue(e.Attrs, nil).(map[string]any)
	}
	return e
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warn"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
