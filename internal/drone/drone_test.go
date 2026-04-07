package drone

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

func testConfig() config.DroneConfig {
	return config.DroneConfig{
		StaleCheckInterval:   50 * time.Millisecond,
		StaleThreshold:       200 * time.Millisecond,
		MaxMessagesPerSecond: 1000,
		RateLimitBurst:       100,
		RateLimitWindow:      time.Second,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeEvent(sysID uint8, payload any) *protocol.TelemetryEvent {
	return &protocol.TelemetryEvent{
		DroneID:    protocol.DroneID{SystemID: sysID, ComponentID: 1},
		MessageID:  0,
		Timestamp:  time.Now(),
		SourceAddr: "10.0.0.1:14550",
		Payload:    payload,
	}
}

// --- State tests ---

func TestState_Clone(t *testing.T) {
	s := &State{
		ID:        protocol.DroneID{SystemID: 1, ComponentID: 1},
		FirstSeen: time.Now(),
		LastSeen:  time.Now(),
		Heartbeat: &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor, Armed: true},
		GPS:       &protocol.GPSPosition{Latitude: 47.0, Longitude: 8.0},
		Battery:   &protocol.BatteryStatus{Remaining: 85},
		Attitude:  &protocol.Attitude{Roll: 0.1},
	}

	clone := s.Clone()

	// Modify original
	s.Heartbeat.Armed = false
	s.GPS.Latitude = 0
	s.Battery.Remaining = 0
	s.Attitude.Roll = 9.9

	// Clone should be unaffected
	if !clone.Heartbeat.Armed {
		t.Error("clone.Heartbeat.Armed was affected by original change")
	}
	if clone.GPS.Latitude != 47.0 {
		t.Error("clone.GPS.Latitude was affected by original change")
	}
	if clone.Battery.Remaining != 85 {
		t.Error("clone.Battery.Remaining was affected by original change")
	}
	if clone.Attitude.Roll != 0.1 {
		t.Error("clone.Attitude.Roll was affected by original change")
	}
}

func TestState_ToSummary(t *testing.T) {
	s := &State{
		ID:          protocol.DroneID{SystemID: 5, ComponentID: 1},
		IsConnected: true,
		IsArmed:     true,
		FlightMode:  "POSITION",
		LastSeen:    time.Now(),
		Heartbeat:   &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor},
		GPS:         &protocol.GPSPosition{Latitude: 47.0, Longitude: 8.0, Altitude: 500, Heading: 90},
		Battery:     &protocol.BatteryStatus{Remaining: 72, VoltageTotal: 22.4},
	}

	sum := s.ToSummary()
	if sum.SystemID != 5 {
		t.Errorf("SystemID = %d, want 5", sum.SystemID)
	}
	if !sum.IsConnected {
		t.Error("should be connected")
	}
	if !sum.IsArmed {
		t.Error("should be armed")
	}
	if sum.FlightMode != "POSITION" {
		t.Errorf("FlightMode = %q, want POSITION", sum.FlightMode)
	}
	if sum.Latitude == nil || *sum.Latitude != 47.0 {
		t.Error("Latitude should be 47.0")
	}
	if sum.BatteryPercent == nil || *sum.BatteryPercent != 72 {
		t.Error("BatteryPercent should be 72")
	}
	if sum.VehicleType != "quadrotor" {
		t.Errorf("VehicleType = %q, want quadrotor", sum.VehicleType)
	}
}

func TestState_ToSummary_NoOptionalFields(t *testing.T) {
	s := &State{
		ID:       protocol.DroneID{SystemID: 1, ComponentID: 1},
		LastSeen: time.Now(),
	}
	sum := s.ToSummary()
	if sum.Latitude != nil {
		t.Error("Latitude should be nil without GPS")
	}
	if sum.BatteryPercent != nil {
		t.Error("BatteryPercent should be nil without battery")
	}
}

// --- Manager tests ---

func TestManager_NewDroneRegistration(t *testing.T) {
	updates := make(chan StateUpdate, 16)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	event := makeEvent(1, &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor})
	mgr.ProcessEvent(event)

	state := mgr.GetState(protocol.DroneID{SystemID: 1, ComponentID: 1})
	if state == nil {
		t.Fatal("drone should be registered")
	}
	if !state.IsConnected {
		t.Error("new drone should be connected")
	}
	if state.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", state.MessageCount)
	}

	mgr.Stop()
}

func TestManager_ArmDisarmDetection(t *testing.T) {
	updates := make(chan StateUpdate, 16)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// First heartbeat: disarmed
	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{Armed: false}))

	// Arm
	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{Armed: true}))

	// Check we got an armed update
	found := false
	for len(updates) > 0 {
		u := <-updates
		if u.Type == UpdateTypeArmed {
			found = true
		}
	}
	if !found {
		t.Error("expected UpdateTypeArmed")
	}

	// Disarm
	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{Armed: false}))
	found = false
	for len(updates) > 0 {
		u := <-updates
		if u.Type == UpdateTypeDisarmed {
			found = true
		}
	}
	if !found {
		t.Error("expected UpdateTypeDisarmed")
	}

	mgr.Stop()
}

func TestManager_StaleDetection(t *testing.T) {
	cfg := testConfig()
	cfg.StaleThreshold = 100 * time.Millisecond
	cfg.StaleCheckInterval = 50 * time.Millisecond

	updates := make(chan StateUpdate, 64)
	mgr := NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{}))

	// Wait for stale detection
	time.Sleep(300 * time.Millisecond)

	state := mgr.GetState(protocol.DroneID{SystemID: 1, ComponentID: 1})
	if state == nil {
		t.Fatal("drone should still exist")
	}
	if state.IsConnected {
		t.Error("drone should be disconnected after stale threshold")
	}

	mgr.Stop()
}

func TestManager_Reconnection(t *testing.T) {
	cfg := testConfig()
	cfg.StaleThreshold = 100 * time.Millisecond
	cfg.StaleCheckInterval = 50 * time.Millisecond

	updates := make(chan StateUpdate, 64)
	mgr := NewManager(cfg, updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{}))
	time.Sleep(300 * time.Millisecond) // go stale

	// Reconnect
	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{}))

	state := mgr.GetState(protocol.DroneID{SystemID: 1, ComponentID: 1})
	if !state.IsConnected {
		t.Error("drone should be reconnected")
	}

	mgr.Stop()
}

func TestManager_AllPayloadTypes(t *testing.T) {
	updates := make(chan StateUpdate, 64)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	id := protocol.DroneID{SystemID: 1, ComponentID: 1}

	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor}))
	mgr.ProcessEvent(makeEvent(1, &protocol.GPSPosition{Latitude: 47.0, Longitude: 8.0}))
	mgr.ProcessEvent(makeEvent(1, &protocol.BatteryStatus{Remaining: 80}))
	mgr.ProcessEvent(makeEvent(1, &protocol.Attitude{Roll: 0.1, Pitch: -0.05}))

	state := mgr.GetState(id)
	if state.GPS == nil || state.GPS.Latitude != 47.0 {
		t.Error("GPS not updated")
	}
	if state.Battery == nil || state.Battery.Remaining != 80 {
		t.Error("Battery not updated")
	}
	if state.Attitude == nil || state.Attitude.Roll != 0.1 {
		t.Error("Attitude not updated")
	}

	mgr.Stop()
}

func TestManager_ConcurrentAccess(t *testing.T) {
	updates := make(chan StateUpdate, 10000)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	var wg sync.WaitGroup
	// Writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id uint8) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mgr.ProcessEvent(makeEvent(id, &protocol.Heartbeat{}))
			}
		}(uint8(i + 1))
	}
	// Readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mgr.GetAllStates()
				mgr.GetAllSummaries()
				mgr.GetConnectedCount()
			}
		}()
	}

	wg.Wait()
	stats := mgr.Stats()
	if stats.TotalDrones != 10 {
		t.Errorf("TotalDrones = %d, want 10", stats.TotalDrones)
	}

	mgr.Stop()
}

func TestManager_GetAllSummaries(t *testing.T) {
	updates := make(chan StateUpdate, 64)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{}))
	mgr.ProcessEvent(makeEvent(2, &protocol.Heartbeat{}))
	mgr.ProcessEvent(makeEvent(3, &protocol.Heartbeat{}))

	summaries := mgr.GetAllSummaries()
	if len(summaries) != 3 {
		t.Errorf("len(summaries) = %d, want 3", len(summaries))
	}

	mgr.Stop()
}

func TestManager_EmitUpdateDropped(t *testing.T) {
	// Tiny channel to force drops
	updates := make(chan StateUpdate, 1)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Fill the channel
	for i := 0; i < 100; i++ {
		mgr.ProcessEvent(makeEvent(1, &protocol.Heartbeat{}))
	}

	if mgr.droppedUpdates.Load() == 0 {
		t.Error("expected some dropped updates with tiny channel")
	}

	mgr.Stop()
}

// --- Flight mode decoding ---

func TestDecodeFlightMode(t *testing.T) {
	tests := []struct {
		name       string
		customMode uint32
		baseMode   uint8
		want       string
	}{
		{"unknown_no_custom", 0, 0, "UNKNOWN"},
		{"manual", 1 << 16, 0x01, "MANUAL"},
		{"altitude", 2 << 16, 0x01, "ALTITUDE"},
		{"position", 3 << 16, 0x01, "POSITION"},
		{"auto_mission", (4 << 16) | (4 << 24), 0x01, "MISSION"},
		{"auto_rtl", (4 << 16) | (5 << 24), 0x01, "RTL"},
		{"auto_land", (4 << 16) | (6 << 24), 0x01, "LAND"},
		{"auto_takeoff", (4 << 16) | (2 << 24), 0x01, "TAKEOFF"},
		{"auto_loiter", (4 << 16) | (3 << 24), 0x01, "LOITER"},
		{"auto_ready", (4 << 16) | (1 << 24), 0x01, "READY"},
		{"acro", 5 << 16, 0x01, "ACRO"},
		{"offboard", 6 << 16, 0x01, "OFFBOARD"},
		{"stabilized", 7 << 16, 0x01, "STABILIZED"},
		{"orbit", (3 << 16) | (3 << 24), 0x01, "ORBIT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeFlightMode(tt.customMode, tt.baseMode)
			if got != tt.want {
				t.Errorf("decodeFlightMode(%d, %d) = %q, want %q", tt.customMode, tt.baseMode, got, tt.want)
			}
		})
	}
}

// --- UpdateType.String ---

func TestUpdateType_String(t *testing.T) {
	tests := []struct {
		t    UpdateType
		want string
	}{
		{UpdateTypeNew, "new"},
		{UpdateTypeTelemetry, "telemetry"},
		{UpdateTypeArmed, "armed"},
		{UpdateTypeDisarmed, "disarmed"},
		{UpdateTypeDisconnect, "disconnect"},
		{UpdateTypeReconnect, "reconnect"},
		{UpdateType(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.t.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.t, got, tt.want)
		}
	}
}

// --- Benchmarks ---

func BenchmarkProcessEvent(b *testing.B) {
	updates := make(chan StateUpdate, 10000)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	event := makeEvent(1, &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor})

	// Drain updates in background
	go func() {
		for range updates {
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mgr.ProcessEvent(event)
	}
	b.StopTimer()
	mgr.Stop()
}

func BenchmarkGetAllSummaries(b *testing.B) {
	updates := make(chan StateUpdate, 10000)
	mgr := NewManager(testConfig(), updates, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Register 50 drones
	for i := uint8(1); i <= 50; i++ {
		mgr.ProcessEvent(makeEvent(i, &protocol.Heartbeat{}))
		mgr.ProcessEvent(makeEvent(i, &protocol.GPSPosition{Latitude: float64(i)}))
	}

	// Drain updates
	go func() {
		for range updates {
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mgr.GetAllSummaries()
	}
	b.StopTimer()
	mgr.Stop()
}
