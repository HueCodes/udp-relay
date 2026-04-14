package drone

import (
	"sync"
	"time"
)

// HistoryEntry represents a snapshot of drone state at a point in time.
type HistoryEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	Alt       float64   `json:"alt"`
	Heading   float64   `json:"heading"`
	Battery   int8      `json:"battery_pct"`
	Armed     bool      `json:"armed"`
	Mode      string    `json:"flight_mode"`
}

// RingBuffer is a fixed-size circular buffer for drone state history.
// It is safe for concurrent read/write from a single writer with multiple readers
// when used under the Manager's RWMutex.
type RingBuffer struct {
	entries []HistoryEntry
	head    int
	count   int
	cap     int
	mu      sync.RWMutex
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		entries: make([]HistoryEntry, capacity),
		cap:     capacity,
	}
}

// Push adds an entry to the ring buffer, overwriting the oldest if full.
func (rb *RingBuffer) Push(entry HistoryEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.entries[rb.head] = entry
	rb.head = (rb.head + 1) % rb.cap
	if rb.count < rb.cap {
		rb.count++
	}
}

// Last returns the most recent n entries, oldest first.
func (rb *RingBuffer) Last(n int) []HistoryEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if n > rb.count {
		n = rb.count
	}
	if n == 0 {
		return nil
	}

	result := make([]HistoryEntry, n)
	start := (rb.head - n + rb.cap) % rb.cap
	for i := 0; i < n; i++ {
		result[i] = rb.entries[(start+i)%rb.cap]
	}
	return result
}

// Len returns the number of entries stored.
func (rb *RingBuffer) Len() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.count
}
