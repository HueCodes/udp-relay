package drone

import (
	"context"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/pkg/protocol"
)

func TestRingBuffer_Basic(t *testing.T) {
	rb := NewRingBuffer(5)

	if rb.Len() != 0 {
		t.Fatalf("expected empty, got %d", rb.Len())
	}

	// Push 3 entries
	for i := 0; i < 3; i++ {
		rb.Push(HistoryEntry{
			Timestamp: time.Now(),
			Lat:       float64(i),
			Lon:       float64(i),
		})
	}

	if rb.Len() != 3 {
		t.Fatalf("expected 3, got %d", rb.Len())
	}

	entries := rb.Last(10) // request more than available
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify order (oldest first)
	for i, e := range entries {
		if e.Lat != float64(i) {
			t.Errorf("entry %d: expected lat %f, got %f", i, float64(i), e.Lat)
		}
	}
}

func TestRingBuffer_Overflow(t *testing.T) {
	rb := NewRingBuffer(3)

	// Push 5 entries into a size-3 buffer
	for i := 0; i < 5; i++ {
		rb.Push(HistoryEntry{Lat: float64(i)})
	}

	if rb.Len() != 3 {
		t.Fatalf("expected 3, got %d", rb.Len())
	}

	entries := rb.Last(3)
	// Should contain entries 2, 3, 4 (oldest overwritten)
	expected := []float64{2, 3, 4}
	for i, e := range entries {
		if e.Lat != expected[i] {
			t.Errorf("entry %d: expected %f, got %f", i, expected[i], e.Lat)
		}
	}
}

func TestRingBuffer_LastSubset(t *testing.T) {
	rb := NewRingBuffer(10)

	for i := 0; i < 10; i++ {
		rb.Push(HistoryEntry{Lat: float64(i)})
	}

	entries := rb.Last(3)
	if len(entries) != 3 {
		t.Fatalf("expected 3, got %d", len(entries))
	}

	// Last 3 should be 7, 8, 9
	expected := []float64{7, 8, 9}
	for i, e := range entries {
		if e.Lat != expected[i] {
			t.Errorf("entry %d: expected %f, got %f", i, expected[i], e.Lat)
		}
	}
}

func TestRingBuffer_Empty(t *testing.T) {
	rb := NewRingBuffer(5)

	entries := rb.Last(5)
	if entries != nil {
		t.Fatalf("expected nil, got %v", entries)
	}

	entries = rb.Last(0)
	if entries != nil {
		t.Fatalf("expected nil for 0, got %v", entries)
	}
}

func TestManagerHistory(t *testing.T) {
	cfg := testConfig()
	cfg.HistorySize = 10
	updates := make(chan StateUpdate, 1000)
	mgr := NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()

	go func() {
		for range updates {
		}
	}()

	// Register drone with GPS
	for i := 0; i < 5; i++ {
		mgr.ProcessEvent(makeEvent(1, &protocol.GPSPosition{
			Latitude:  37.7749 + float64(i)*0.001,
			Longitude: -122.4194,
			Altitude:  float64(50 + i),
		}))
	}

	entries := mgr.GetHistory(1, 10)
	if len(entries) != 5 {
		t.Fatalf("expected 5 history entries, got %d", len(entries))
	}

	// First entry should be oldest
	if entries[0].Alt != 50 {
		t.Errorf("expected first alt 50, got %f", entries[0].Alt)
	}
	if entries[4].Alt != 54 {
		t.Errorf("expected last alt 54, got %f", entries[4].Alt)
	}

	// Non-existent drone
	entries = mgr.GetHistory(99, 10)
	if entries != nil {
		t.Errorf("expected nil for non-existent drone")
	}
}

func TestGetStateBySystemID(t *testing.T) {
	updates := make(chan StateUpdate, 1000)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()

	go func() {
		for range updates {
		}
	}()

	mgr.ProcessEvent(makeEvent(42, &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor}))

	state := mgr.GetStateBySystemID(42)
	if state == nil {
		t.Fatal("expected state for drone 42")
	}
	if state.ID.SystemID != 42 {
		t.Errorf("expected system ID 42, got %d", state.ID.SystemID)
	}

	state = mgr.GetStateBySystemID(99)
	if state != nil {
		t.Error("expected nil for non-existent drone")
	}
}
