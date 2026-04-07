package drone

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// Manager maintains the registry of all known drones and their states.
// It provides thread-safe access using RWMutex for high read concurrency.
//
// Design rationale:
//   - RWMutex is used because reads (state queries, WebSocket broadcasts)
//     vastly outnumber writes (telemetry updates).
//   - State is stored by value with pointer fields to allow cheap snapshots.
//   - Stale drone detection runs in a background goroutine.
type Manager struct {
	cfg    config.DroneConfig
	logger *slog.Logger

	// Protected by mu
	mu     sync.RWMutex
	drones map[protocol.DroneID]*State

	// Channel for state update notifications (fan-out)
	updates chan<- StateUpdate

	// Metrics
	droppedUpdates atomic.Uint64

	// For graceful shutdown
	done chan struct{}
	wg   sync.WaitGroup
}

// NewManager creates a new drone manager.
func NewManager(cfg config.DroneConfig, updates chan<- StateUpdate, logger *slog.Logger) *Manager {
	return &Manager{
		cfg:     cfg,
		logger:  logger.With("component", "drone_manager"),
		drones:  make(map[protocol.DroneID]*State),
		updates: updates,
		done:    make(chan struct{}),
	}
}

// Start begins background tasks (stale drone detection).
func (m *Manager) Start(ctx context.Context) {
	m.wg.Add(1)
	go m.staleChecker(ctx)
	m.logger.Info("drone manager started",
		"stale_threshold", m.cfg.StaleThreshold,
		"check_interval", m.cfg.StaleCheckInterval)
}

// Stop gracefully shuts down the manager.
func (m *Manager) Stop() {
	close(m.done)
	m.wg.Wait()
	m.logger.Info("drone manager stopped", "total_drones", len(m.drones))
}

// ProcessEvent handles an incoming telemetry event.
// This is the main entry point from the ingest pipeline.
func (m *Manager) ProcessEvent(event *protocol.TelemetryEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, exists := m.drones[event.DroneID]
	now := event.Timestamp

	if !exists {
		// New drone registration
		state = &State{
			ID:          event.DroneID,
			SourceAddr:  event.SourceAddr,
			FirstSeen:   now,
			LastSeen:    now,
			IsConnected: true,
		}
		m.drones[event.DroneID] = state

		m.logger.Info("new drone registered",
			"system_id", event.DroneID.SystemID,
			"component_id", event.DroneID.ComponentID,
			"source", event.SourceAddr)

		// Notify subscribers
		m.emitUpdate(StateUpdate{
			DroneID:   event.DroneID,
			Timestamp: now,
			Type:      UpdateTypeNew,
			State:     state.Clone(),
		})
	}

	// Check for reconnection
	if !state.IsConnected {
		state.IsConnected = true
		m.emitUpdate(StateUpdate{
			DroneID:   event.DroneID,
			Timestamp: now,
			Type:      UpdateTypeReconnect,
			State:     state.Clone(),
		})
	}

	// Update common fields
	state.LastSeen = now
	state.SourceAddr = event.SourceAddr
	state.MessageCount++

	// Update type-specific state
	var updateType UpdateType = UpdateTypeTelemetry
	previouslyArmed := state.IsArmed

	switch payload := event.Payload.(type) {
	case *protocol.Heartbeat:
		state.Heartbeat = payload
		state.LastHeartbeat = now
		state.IsArmed = payload.Armed
		state.FlightMode = decodeFlightMode(payload.CustomMode, payload.BaseMode)

		// Detect arm/disarm state changes
		if payload.Armed && !previouslyArmed {
			updateType = UpdateTypeArmed
			m.logger.Info("drone armed",
				"system_id", event.DroneID.SystemID)
		} else if !payload.Armed && previouslyArmed {
			updateType = UpdateTypeDisarmed
			m.logger.Info("drone disarmed",
				"system_id", event.DroneID.SystemID)
		}

	case *protocol.GPSPosition:
		state.GPS = payload

	case *protocol.BatteryStatus:
		state.Battery = payload

	case *protocol.Attitude:
		state.Attitude = payload
	}

	// Emit update notification
	m.emitUpdate(StateUpdate{
		DroneID:   event.DroneID,
		Timestamp: now,
		Type:      updateType,
		State:     state.Clone(),
	})
}

// emitUpdate sends an update to subscribers (non-blocking).
func (m *Manager) emitUpdate(update StateUpdate) {
	if m.updates == nil {
		return
	}

	select {
	case m.updates <- update:
	default:
		dropped := m.droppedUpdates.Add(1)
		if dropped%1000 == 1 {
			m.logger.Warn("update channel full, dropping state update",
				"drone_id", update.DroneID.SystemID,
				"update_type", update.Type.String(),
				"total_dropped", dropped)
		}
	}
}

// GetState returns a copy of a drone's state.
// Returns nil if the drone is not registered.
func (m *Manager) GetState(id protocol.DroneID) *State {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state, exists := m.drones[id]
	if !exists {
		return nil
	}

	clone := state.Clone()
	return &clone
}

// GetAllStates returns a copy of all drone states.
func (m *Manager) GetAllStates() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()

	states := make([]State, 0, len(m.drones))
	for _, state := range m.drones {
		states = append(states, state.Clone())
	}
	return states
}

// GetAllSummaries returns lightweight summaries of all drones.
// Optimized for WebSocket broadcasts.
func (m *Manager) GetAllSummaries() []Summary {
	m.mu.RLock()
	defer m.mu.RUnlock()

	summaries := make([]Summary, 0, len(m.drones))
	for _, state := range m.drones {
		summaries = append(summaries, state.ToSummary())
	}
	return summaries
}

// GetConnectedCount returns the number of connected drones.
func (m *Manager) GetConnectedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, state := range m.drones {
		if state.IsConnected {
			count++
		}
	}
	return count
}

// staleChecker periodically checks for and marks stale drones.
func (m *Manager) staleChecker(ctx context.Context) {
	defer m.wg.Done()

	for {
		if !m.runStaleChecker(ctx) {
			return
		}
	}
}

func (m *Manager) runStaleChecker(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("stale checker panicked, restarting",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			panicked = true
		}
	}()

	ticker := time.NewTicker(m.cfg.StaleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-m.done:
			return false
		case <-ticker.C:
			m.checkStale()
		}
	}
}

// checkStale marks drones as disconnected if they haven't been seen recently.
func (m *Manager) checkStale() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	threshold := m.cfg.StaleThreshold

	for id, state := range m.drones {
		if state.IsConnected && now.Sub(state.LastSeen) > threshold {
			state.IsConnected = false

			m.logger.Warn("drone disconnected (stale)",
				"system_id", id.SystemID,
				"last_seen", state.LastSeen,
				"threshold", threshold)

			m.emitUpdate(StateUpdate{
				DroneID:   id,
				Timestamp: now,
				Type:      UpdateTypeDisconnect,
				State:     state.Clone(),
			})
		}
	}
}

// decodeFlightMode converts PX4 custom mode to a human-readable string.
// This is a simplified version; full implementation would handle all PX4 modes.
func decodeFlightMode(customMode uint32, baseMode uint8) string {
	// PX4 main mode is in bits 16-23, sub mode in bits 24-31
	mainMode := (customMode >> 16) & 0xFF
	subMode := (customMode >> 24) & 0xFF

	// Check if custom mode is enabled
	if baseMode&0x01 == 0 {
		return "UNKNOWN"
	}

	switch mainMode {
	case 1: // Manual
		return "MANUAL"
	case 2: // Altitude
		return "ALTITUDE"
	case 3: // Position
		switch subMode {
		case 0:
			return "POSITION"
		case 1:
			return "POSCTL"
		case 3:
			return "ORBIT"
		default:
			return "POSITION"
		}
	case 4: // Auto
		switch subMode {
		case 1:
			return "READY"
		case 2:
			return "TAKEOFF"
		case 3:
			return "LOITER"
		case 4:
			return "MISSION"
		case 5:
			return "RTL"
		case 6:
			return "LAND"
		default:
			return "AUTO"
		}
	case 5: // Acro
		return "ACRO"
	case 6: // Offboard
		return "OFFBOARD"
	case 7: // Stabilized
		return "STABILIZED"
	default:
		return "UNKNOWN"
	}
}

// Stats returns manager statistics.
func (m *Manager) Stats() ManagerStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := ManagerStats{
		TotalDrones:     len(m.drones),
		ConnectedDrones: 0,
		ArmedDrones:     0,
	}

	for _, state := range m.drones {
		if state.IsConnected {
			stats.ConnectedDrones++
		}
		if state.IsArmed {
			stats.ArmedDrones++
		}
		stats.TotalMessages += state.MessageCount
	}

	return stats
}

// ManagerStats contains drone manager statistics.
type ManagerStats struct {
	TotalDrones     int
	ConnectedDrones int
	ArmedDrones     int
	TotalMessages   uint64
}
