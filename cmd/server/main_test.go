package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestConfigureLogger_Levels(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo}, // default
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			logger := configureLogger(tt.level, "text")
			if !logger.Enabled(context.Background(), tt.want) {
				t.Errorf("logger not enabled at %v for level=%q", tt.want, tt.level)
			}
		})
	}
}

func TestConfigureLogger_Formats(t *testing.T) {
	// Should not panic for either format
	configureLogger("info", "text")
	configureLogger("info", "json")
}

func TestEventProcessor_RoutesEvents(t *testing.T) {
	updates := make(chan drone.StateUpdate, 64)
	cfg := config.DroneConfig{
		StaleCheckInterval: 10 * time.Second,
		StaleThreshold:     30 * time.Second,
	}
	mgr := drone.NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	events := make(chan *protocol.TelemetryEvent, 10)

	go eventProcessor(ctx, events, mgr, discardLogger())

	// Send an event
	events <- &protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: 5, ComponentID: 1},
		Timestamp: time.Now(),
		Payload:   &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor},
	}

	// Give it time to process
	time.Sleep(50 * time.Millisecond)

	state := mgr.GetState(protocol.DroneID{SystemID: 5, ComponentID: 1})
	if state == nil {
		t.Fatal("event processor did not route event to manager")
	}

	mgr.Stop()
}

func TestEventProcessor_StopsOnContextCancel(t *testing.T) {
	updates := make(chan drone.StateUpdate, 64)
	cfg := config.DroneConfig{
		StaleCheckInterval: 10 * time.Second,
		StaleThreshold:     30 * time.Second,
	}
	mgr := drone.NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)

	events := make(chan *protocol.TelemetryEvent, 10)
	done := make(chan struct{})
	go func() {
		eventProcessor(ctx, events, mgr, discardLogger())
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// stopped
	case <-time.After(2 * time.Second):
		t.Fatal("event processor did not stop on context cancel")
	}

	mgr.Stop()
}

func TestEventProcessor_StopsOnChannelClose(t *testing.T) {
	updates := make(chan drone.StateUpdate, 64)
	cfg := config.DroneConfig{
		StaleCheckInterval: 10 * time.Second,
		StaleThreshold:     30 * time.Second,
	}
	mgr := drone.NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	events := make(chan *protocol.TelemetryEvent, 10)
	done := make(chan struct{})
	go func() {
		eventProcessor(ctx, events, mgr, discardLogger())
		close(done)
	}()

	close(events)

	select {
	case <-done:
		// stopped
	case <-time.After(2 * time.Second):
		t.Fatal("event processor did not stop on channel close")
	}

	mgr.Stop()
}

func TestRunEventProcessor_PanicRecovery(t *testing.T) {
	// This tests that runEventProcessor returns panicked=true on panic
	updates := make(chan drone.StateUpdate, 64)
	cfg := config.DroneConfig{
		StaleCheckInterval: 10 * time.Second,
		StaleThreshold:     30 * time.Second,
	}
	mgr := drone.NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Send a nil event which will cause ProcessEvent to panic
	events := make(chan *protocol.TelemetryEvent, 1)
	events <- nil

	panicked := runEventProcessor(ctx, events, mgr, discardLogger())
	if !panicked {
		t.Error("expected panicked=true after nil event")
	}

	mgr.Stop()
}
