// Package drone provides drone state management and registry.
package drone

import (
	"time"

	"github.com/hugh/go-drone-server/pkg/protocol"
)

// State represents the last known state of a drone.
// This structure is designed to be cheaply copyable for snapshot operations.
type State struct {
	// Identity
	ID         protocol.DroneID
	SourceAddr string // Last known UDP source address

	// Timestamps
	FirstSeen    time.Time
	LastSeen     time.Time
	LastHeartbeat time.Time

	// Core telemetry
	Heartbeat *protocol.Heartbeat
	GPS       *protocol.GPSPosition
	Battery   *protocol.BatteryStatus
	Attitude  *protocol.Attitude

	// Derived state
	IsConnected bool // True if recently seen
	IsArmed     bool // Derived from heartbeat
	FlightMode  string

	// Statistics
	MessageCount uint64
}

// Clone creates a deep copy of the drone state.
// This is used when we need to return state without holding locks.
func (s *State) Clone() State {
	clone := *s

	// Deep copy embedded structs
	if s.Heartbeat != nil {
		hb := *s.Heartbeat
		clone.Heartbeat = &hb
	}
	if s.GPS != nil {
		gps := *s.GPS
		clone.GPS = &gps
	}
	if s.Battery != nil {
		bat := *s.Battery
		clone.Battery = &bat
	}
	if s.Attitude != nil {
		att := *s.Attitude
		clone.Attitude = &att
	}

	return clone
}

// Summary returns a lightweight summary of the drone state.
// Used for WebSocket broadcasts to minimize payload size.
type Summary struct {
	SystemID    uint8   `json:"system_id"`
	ComponentID uint8   `json:"component_id"`
	IsConnected bool    `json:"connected"`
	IsArmed     bool    `json:"armed"`
	FlightMode  string  `json:"flight_mode,omitempty"`
	VehicleType string  `json:"vehicle_type,omitempty"`

	// Position (nullable)
	Latitude  *float64 `json:"lat,omitempty"`
	Longitude *float64 `json:"lon,omitempty"`
	Altitude  *float64 `json:"alt,omitempty"`
	Heading   *float64 `json:"heading,omitempty"`

	// Battery (nullable)
	BatteryPercent *int8    `json:"battery_pct,omitempty"`
	BatteryVoltage *float64 `json:"battery_v,omitempty"`

	// Timing
	LastSeenMs int64 `json:"last_seen_ms"`
}

// ToSummary converts the full state to a lightweight summary.
func (s *State) ToSummary() Summary {
	sum := Summary{
		SystemID:    s.ID.SystemID,
		ComponentID: s.ID.ComponentID,
		IsConnected: s.IsConnected,
		IsArmed:     s.IsArmed,
		FlightMode:  s.FlightMode,
		LastSeenMs:  s.LastSeen.UnixMilli(),
	}

	if s.Heartbeat != nil {
		sum.VehicleType = s.Heartbeat.Type.String()
	}

	if s.GPS != nil {
		sum.Latitude = &s.GPS.Latitude
		sum.Longitude = &s.GPS.Longitude
		sum.Altitude = &s.GPS.Altitude
		sum.Heading = &s.GPS.Heading
	}

	if s.Battery != nil {
		if s.Battery.Remaining >= 0 {
			sum.BatteryPercent = &s.Battery.Remaining
		}
		if s.Battery.VoltageTotal > 0 {
			sum.BatteryVoltage = &s.Battery.VoltageTotal
		}
	}

	return sum
}

// StateUpdate represents a change to drone state.
// Used for event-driven notifications.
type StateUpdate struct {
	DroneID   protocol.DroneID
	Timestamp time.Time
	Type      UpdateType
	State     State // Copy of current state
}

// UpdateType categorizes state updates.
type UpdateType int

// State update types for event-driven notifications.
const (
	UpdateTypeNew        UpdateType = iota // New drone registered
	UpdateTypeTelemetry                    // Regular telemetry update
	UpdateTypeArmed                        // Arm state changed
	UpdateTypeDisarmed                     // Disarm state changed
	UpdateTypeDisconnect                   // Drone went stale
	UpdateTypeReconnect                    // Drone came back online
)

func (t UpdateType) String() string {
	switch t {
	case UpdateTypeNew:
		return "new"
	case UpdateTypeTelemetry:
		return "telemetry"
	case UpdateTypeArmed:
		return "armed"
	case UpdateTypeDisarmed:
		return "disarmed"
	case UpdateTypeDisconnect:
		return "disconnect"
	case UpdateTypeReconnect:
		return "reconnect"
	default:
		return "unknown"
	}
}
