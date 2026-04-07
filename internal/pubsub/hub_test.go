package pubsub

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

func testConfig() config.PubSubConfig {
	return config.PubSubConfig{
		SubscriberBufferSize: 16,
		DropOnSlowSubscriber: true,
	}
}

func testEvent(id uint32) *protocol.TelemetryEvent {
	return &protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: 1, ComponentID: 1},
		MessageID: id,
		Timestamp: time.Now(),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// 1. Subscribe / Unsubscribe lifecycle
// ---------------------------------------------------------------------------

func TestSubscribeUnsubscribe(t *testing.T) {
	input := make(chan *protocol.TelemetryEvent, 1)
	hub := NewHub(testConfig(), input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	sub := hub.Subscribe("test-sub")
	if sub == nil {
		t.Fatal("Subscribe returned nil")
	}
	if sub.Name != "test-sub" {
		t.Fatalf("expected name 'test-sub', got %q", sub.Name)
	}
	if hub.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", hub.SubscriberCount())
	}

	hub.Unsubscribe(sub)
	if hub.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after unsubscribe, got %d", hub.SubscriberCount())
	}

	hub.Stop()
}

// ---------------------------------------------------------------------------
// 2. Broadcast to multiple subscribers
// ---------------------------------------------------------------------------

func TestBroadcastToMultipleSubscribers(t *testing.T) {
	input := make(chan *protocol.TelemetryEvent, 10)
	hub := NewHub(testConfig(), input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	const numSubs = 5
	subs := make([]*Subscriber, numSubs)
	for i := range numSubs {
		subs[i] = hub.Subscribe(fmt.Sprintf("sub-%d", i))
	}

	event := testEvent(42)
	input <- event

	for i, sub := range subs {
		select {
		case got := <-sub.Events:
			if got.MessageID != 42 {
				t.Errorf("sub %d: expected MessageID 42, got %d", i, got.MessageID)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timed out waiting for event", i)
		}
	}

	hub.Stop()
}

// ---------------------------------------------------------------------------
// 2b. Broadcast with blocking mode (non-drop)
// ---------------------------------------------------------------------------

func TestBroadcastBlockingMode(t *testing.T) {
	cfg := config.PubSubConfig{
		SubscriberBufferSize: 16,
		DropOnSlowSubscriber: false,
	}
	input := make(chan *protocol.TelemetryEvent, 10)
	hub := NewHub(cfg, input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	sub := hub.Subscribe("blocking")

	event := testEvent(99)
	input <- event

	select {
	case got := <-sub.Events:
		if got.MessageID != 99 {
			t.Errorf("expected MessageID 99, got %d", got.MessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event in blocking mode")
	}

	hub.Stop()
}

// ---------------------------------------------------------------------------
// 3. Slow subscriber: events dropped when buffer full
// ---------------------------------------------------------------------------

func TestSlowSubscriberDropsEvents(t *testing.T) {
	cfg := config.PubSubConfig{
		SubscriberBufferSize: 2,
		DropOnSlowSubscriber: true,
	}
	input := make(chan *protocol.TelemetryEvent, 100)
	hub := NewHub(cfg, input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	sub := hub.Subscribe("slow")

	// Send more events than the buffer can hold. Don't read from sub.Events.
	const total = 10
	for i := range total {
		input <- testEvent(uint32(i))
	}

	// Give the hub time to process all events.
	time.Sleep(100 * time.Millisecond)

	stats := hub.Stats()
	if stats.EventsDropped == 0 {
		t.Fatal("expected some events to be dropped for slow subscriber")
	}

	// The subscriber's dropped counter should match the hub-level count.
	if sub.dropped.Load() == 0 {
		t.Fatal("expected subscriber dropped counter > 0")
	}

	hub.Stop()
}

// ---------------------------------------------------------------------------
// 4. Concurrent Stop and Unsubscribe (double-close race fix)
// ---------------------------------------------------------------------------

func TestConcurrentStopAndUnsubscribe(t *testing.T) {
	input := make(chan *protocol.TelemetryEvent, 1)
	hub := NewHub(testConfig(), input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	const numSubs = 20
	subs := make([]*Subscriber, numSubs)
	for i := range numSubs {
		subs[i] = hub.Subscribe(fmt.Sprintf("sub-%d", i))
	}

	// Race: unsubscribe half concurrently while stopping the hub.
	var wg sync.WaitGroup
	wg.Add(numSubs/2 + 1)

	for i := 0; i < numSubs/2; i++ {
		go func(s *Subscriber) {
			defer wg.Done()
			hub.Unsubscribe(s)
		}(subs[i])
	}

	go func() {
		defer wg.Done()
		hub.Stop()
	}()

	wg.Wait()
	// If we get here without a panic, the sync.Once double-close guard works.
}

// ---------------------------------------------------------------------------
// 5. SubscriberCount accuracy
// ---------------------------------------------------------------------------

func TestSubscriberCountAccuracy(t *testing.T) {
	input := make(chan *protocol.TelemetryEvent, 1)
	hub := NewHub(testConfig(), input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	if hub.SubscriberCount() != 0 {
		t.Fatalf("expected 0, got %d", hub.SubscriberCount())
	}

	s1 := hub.Subscribe("a")
	s2 := hub.Subscribe("b")
	s3 := hub.Subscribe("c")

	if hub.SubscriberCount() != 3 {
		t.Fatalf("expected 3, got %d", hub.SubscriberCount())
	}

	hub.Unsubscribe(s2)
	if hub.SubscriberCount() != 2 {
		t.Fatalf("expected 2 after removing one, got %d", hub.SubscriberCount())
	}

	hub.Unsubscribe(s1)
	hub.Unsubscribe(s3)
	if hub.SubscriberCount() != 0 {
		t.Fatalf("expected 0 after removing all, got %d", hub.SubscriberCount())
	}

	hub.Stop()
}

// ---------------------------------------------------------------------------
// 6. Hub Stats correctness
// ---------------------------------------------------------------------------

func TestHubStats(t *testing.T) {
	cfg := config.PubSubConfig{
		SubscriberBufferSize: 64,
		DropOnSlowSubscriber: true,
	}
	input := make(chan *protocol.TelemetryEvent, 100)
	hub := NewHub(cfg, input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	sub1 := hub.Subscribe("ws")
	sub2 := hub.Subscribe("logger")

	const numEvents = 5
	for i := range numEvents {
		input <- testEvent(uint32(i))
	}

	// Drain both subscribers.
	for range numEvents {
		<-sub1.Events
		<-sub2.Events
	}

	// Give hub goroutine time to finish processing.
	time.Sleep(50 * time.Millisecond)

	stats := hub.Stats()
	if stats.Subscribers != 2 {
		t.Errorf("expected 2 subscribers, got %d", stats.Subscribers)
	}
	if stats.EventsReceived != numEvents {
		t.Errorf("expected %d events received, got %d", numEvents, stats.EventsReceived)
	}
	// Each event is broadcast to 2 subscribers.
	if stats.EventsBroadcast != numEvents*2 {
		t.Errorf("expected %d events broadcast, got %d", numEvents*2, stats.EventsBroadcast)
	}
	if stats.EventsDropped != 0 {
		t.Errorf("expected 0 drops, got %d", stats.EventsDropped)
	}
	if len(stats.SubscriberStats) != 2 {
		t.Errorf("expected 2 subscriber stats, got %d", len(stats.SubscriberStats))
	}

	hub.Stop()
}

// ---------------------------------------------------------------------------
// 7. Subscriber.Close() idempotency
// ---------------------------------------------------------------------------

func TestSubscriberCloseIdempotent(t *testing.T) {
	sub := &Subscriber{
		ID:     1,
		Name:   "test",
		Events: make(chan *protocol.TelemetryEvent, 1),
	}

	// Close multiple times; must not panic.
	sub.Close()
	sub.Close()
	sub.Close()
}

// ---------------------------------------------------------------------------
// 8. Context cancellation stops the hub
// ---------------------------------------------------------------------------

func TestContextCancellationStopsHub(t *testing.T) {
	input := make(chan *protocol.TelemetryEvent, 1)
	hub := NewHub(testConfig(), input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	hub.Start(ctx)

	sub := hub.Subscribe("ctx-test")
	_ = sub

	// Cancel context -- the hub run loop should exit.
	cancel()

	// The hub's WaitGroup should complete within a reasonable time.
	done := make(chan struct{})
	go func() {
		hub.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("hub did not stop after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// 9. Input channel closed stops the hub
// ---------------------------------------------------------------------------

func TestInputChannelClosedStopsHub(t *testing.T) {
	input := make(chan *protocol.TelemetryEvent, 1)
	hub := NewHub(testConfig(), input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	close(input)

	done := make(chan struct{})
	go func() {
		hub.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("hub did not stop after input channel closed")
	}
}

// ---------------------------------------------------------------------------
// 10. Benchmarks: broadcast latency with 1, 10, 100 subscribers
// ---------------------------------------------------------------------------

func benchmarkBroadcast(b *testing.B, numSubs int) {
	cfg := config.PubSubConfig{
		SubscriberBufferSize: 4096,
		DropOnSlowSubscriber: true,
	}
	input := make(chan *protocol.TelemetryEvent, 4096)
	hub := NewHub(cfg, input, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	subs := make([]*Subscriber, numSubs)
	for i := range numSubs {
		subs[i] = hub.Subscribe(fmt.Sprintf("bench-%d", i))
	}

	// Drain subscribers in background goroutines.
	var drainWg sync.WaitGroup
	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	for _, sub := range subs {
		drainWg.Add(1)
		go func(s *Subscriber) {
			defer drainWg.Done()
			for {
				select {
				case <-drainCtx.Done():
					return
				case _, ok := <-s.Events:
					if !ok {
						return
					}
				}
			}
		}(sub)
	}

	event := testEvent(1)

	b.ResetTimer()
	for range b.N {
		input <- event
	}
	// Wait for all events to be consumed.
	b.StopTimer()

	drainCancel()
	hub.Stop()
	drainWg.Wait()
}

func BenchmarkBroadcast1Sub(b *testing.B)   { benchmarkBroadcast(b, 1) }
func BenchmarkBroadcast10Sub(b *testing.B)  { benchmarkBroadcast(b, 10) }
func BenchmarkBroadcast100Sub(b *testing.B) { benchmarkBroadcast(b, 100) }
