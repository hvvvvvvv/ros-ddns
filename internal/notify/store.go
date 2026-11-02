// Package notify stores bounded in-app notifications. Event detection belongs
// to the caller; the store does not monitor service snapshots.
package notify

import (
	"sync"
	"time"
)

type Event struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Item     string    `json:"item,omitempty"`
	Phase    string    `json:"phase,omitempty"`
	Message  string    `json:"message"`
}

// Store is safe for concurrent use and starts no goroutines. Create it with
// NewStore. Callers supply notification contents, including any redaction.
type Store struct {
	mu          sync.Mutex
	ring        []Event
	start       int
	size        int
	id          uint64
	subscribers map[chan Event]uint64
	closed      bool
}

func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = 200
	}
	return &Store{ring: make([]Event, capacity), subscribers: make(map[chan Event]uint64)}
}

// Add assigns a new ID and fills a zero Time with the current time. Caller IDs
// are ignored. After Close, Add is a no-op.
func (s *Store) Add(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.id++
	event.ID = s.id
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	if s.size == len(s.ring) {
		s.ring[s.start] = event
		s.start = (s.start + 1) % len(s.ring)
	} else {
		s.ring[(s.start+s.size)%len(s.ring)] = event
		s.size++
	}
	for ch, after := range s.subscribers {
		if event.ID <= after {
			continue
		}
		select {
		case ch <- event:
		default:
			select {
			case <-ch:
			default:
			}
			// Sends and closes share the lock; readers can only free space.
			ch <- event
		}
	}
}

// History returns the newest limit retained events in chronological ID order.
// A nonpositive limit returns all retained events. The returned slice is owned
// by the caller and may be modified independently of the store.
func (s *Store) History(limit int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > s.size {
		limit = s.size
	}
	out := make([]Event, 0, limit)
	for i := s.size - limit; i < s.size; i++ {
		out = append(out, s.ring[(s.start+i)%len(s.ring)])
	}
	return out
}

// Subscribe queues retained events with IDs greater than after, then delivers
// live events newer than after. The buffer has the ring's capacity. A full
// buffer drops its oldest event so Add never waits for a consumer. ID gaps
// indicate loss. Cancel is idempotent and closes the channel; buffered events
// remain readable. Subscribing after Close returns an empty, closed channel.
func (s *Store) Subscribe(after uint64) (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, len(s.ring))
	if s.closed {
		close(ch)
		return ch, func() {}
	}
	for i := 0; i < s.size; i++ {
		e := s.ring[(s.start+i)%len(s.ring)]
		if e.ID > after {
			ch <- e
		}
	}
	s.subscribers[ch] = after
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subscribers[ch]; ok {
			delete(s.subscribers, ch)
			close(ch)
		}
	}
}

// Close stops additions and closes all subscriptions. History stays readable.
// Repeated calls are harmless.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, ch)
	}
}
