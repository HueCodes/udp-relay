// Package pubsub provides a fan-out hub for distributing telemetry events
// to multiple subscribers (WebSocket broadcaster, storage logger, etc.).
package pubsub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// Hub implements the fan-out pattern for distributing telemetry events.
// It receives events from a single input channel and broadcasts them
// to all registered subscribers.
//
// Design rationale:
//   - Subscribers are managed with a mutex to allow dynamic registration.
//   - Each subscriber has a buffered channel to absorb bursts.
//   - Slow subscribers receive dropped messages (configurable) to prevent
//     backpressure from propagating to the ingest pipeline.
type Hub struct {
	cfg    config.PubSubConfig
	logger *slog.Logger

	// Input channel for telemetry events
	input <-chan *protocol.TelemetryEvent

	// Subscriber management
	mu          sync.RWMutex
	subscribers map[string]*Subscriber
	nextID      atomic.Uint64

	// Lifecycle
	done chan struct{}
	wg   sync.WaitGroup

	// Metrics
	eventsReceived   atomic.Uint64
	eventsBroadcast  atomic.Uint64
	eventsDropped    atomic.Uint64
}

// NewHub creates a new fan-out hub.
func NewHub(cfg config.PubSubConfig, input <-chan *protocol.TelemetryEvent, logger *slog.Logger) *Hub {
	return &Hub{
		cfg:         cfg,
		logger:      logger.With("component", "pubsub_hub"),
		input:       input,
		subscribers: make(map[string]*Subscriber),
		done:        make(chan struct{}),
	}
}

// Start begins processing events and broadcasting to subscribers.
func (h *Hub) Start(ctx context.Context) {
	h.wg.Add(1)
	go h.run(ctx)
	h.logger.Info("pub/sub hub started",
		"buffer_size", h.cfg.SubscriberBufferSize,
		"drop_on_slow", h.cfg.DropOnSlowSubscriber)
}

// Stop gracefully shuts down the hub.
func (h *Hub) Stop() {
	close(h.done)
	h.wg.Wait()

	// Close all subscriber channels
	h.mu.Lock()
	for _, sub := range h.subscribers {
		close(sub.Events)
	}
	h.subscribers = make(map[string]*Subscriber)
	h.mu.Unlock()

	h.logger.Info("pub/sub hub stopped",
		"events_received", h.eventsReceived.Load(),
		"events_broadcast", h.eventsBroadcast.Load(),
		"events_dropped", h.eventsDropped.Load())
}

// Subscribe registers a new subscriber and returns their event channel.
// The name is used for logging and debugging.
func (h *Hub) Subscribe(name string) *Subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()

	id := h.nextID.Add(1)
	sub := &Subscriber{
		ID:     id,
		Name:   name,
		Events: make(chan *protocol.TelemetryEvent, h.cfg.SubscriberBufferSize),
	}

	// Use ID as map key to allow multiple subscribers with same name
	key := name
	if _, exists := h.subscribers[key]; exists {
		key = name + "_" + string(rune(id))
	}
	h.subscribers[key] = sub

	h.logger.Info("subscriber registered",
		"name", name,
		"id", id,
		"total_subscribers", len(h.subscribers))

	return sub
}

// Unsubscribe removes a subscriber.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for key, s := range h.subscribers {
		if s.ID == sub.ID {
			delete(h.subscribers, key)
			close(s.Events)
			h.logger.Info("subscriber unregistered",
				"name", sub.Name,
				"id", sub.ID,
				"remaining_subscribers", len(h.subscribers))
			return
		}
	}
}

// SubscriberCount returns the current number of subscribers.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// run is the main event loop.
func (h *Hub) run(ctx context.Context) {
	defer h.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case event, ok := <-h.input:
			if !ok {
				h.logger.Debug("input channel closed")
				return
			}
			h.broadcast(event)
		}
	}
}

// broadcast sends an event to all subscribers.
func (h *Hub) broadcast(event *protocol.TelemetryEvent) {
	h.eventsReceived.Add(1)

	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, sub := range h.subscribers {
		if h.cfg.DropOnSlowSubscriber {
			// Non-blocking send - drop if subscriber is slow
			select {
			case sub.Events <- event:
				h.eventsBroadcast.Add(1)
			default:
				h.eventsDropped.Add(1)
				sub.dropped.Add(1)
			}
		} else {
			// Blocking send - can cause backpressure
			sub.Events <- event
			h.eventsBroadcast.Add(1)
		}
	}
}

// Stats returns hub statistics.
func (h *Hub) Stats() HubStats {
	h.mu.RLock()
	defer h.mu.RUnlock()

	stats := HubStats{
		Subscribers:     len(h.subscribers),
		EventsReceived:  h.eventsReceived.Load(),
		EventsBroadcast: h.eventsBroadcast.Load(),
		EventsDropped:   h.eventsDropped.Load(),
		SubscriberStats: make([]SubscriberStats, 0, len(h.subscribers)),
	}

	for _, sub := range h.subscribers {
		stats.SubscriberStats = append(stats.SubscriberStats, SubscriberStats{
			ID:      sub.ID,
			Name:    sub.Name,
			Pending: len(sub.Events),
			Dropped: sub.dropped.Load(),
		})
	}

	return stats
}

// HubStats contains hub statistics.
type HubStats struct {
	Subscribers     int
	EventsReceived  uint64
	EventsBroadcast uint64
	EventsDropped   uint64
	SubscriberStats []SubscriberStats
}

// SubscriberStats contains per-subscriber statistics.
type SubscriberStats struct {
	ID      uint64
	Name    string
	Pending int    // Events waiting in buffer
	Dropped uint64 // Events dropped due to slow consumption
}

// Subscriber represents a registered event consumer.
type Subscriber struct {
	ID     uint64
	Name   string
	Events chan *protocol.TelemetryEvent

	// Metrics
	dropped atomic.Uint64
}
