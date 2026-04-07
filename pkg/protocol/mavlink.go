// Package protocol provides public MAVLink protocol constants and types.
// This package is safe for external consumption and contains no internal dependencies.
package protocol

import "time"

// MAVLink v2 frame structure constants
const (
	// Frame markers and sizes
	MagicV1      byte = 0xFE // MAVLink v1 start marker
	MagicV2      byte = 0xFD // MAVLink v2 start marker
	HeaderSizeV2      = 10   // MAVLink v2 header size (excluding STX)
	ChecksumSize      = 2    // CRC-16 checksum size
	MaxPayloadSize    = 255  // Maximum payload length

	// Header field offsets (after STX byte)
	OffsetPayloadLen   = 0
	OffsetIncompatFlag = 1
	OffsetCompatFlag   = 2
	OffsetSequence     = 3
	OffsetSystemID     = 4
	OffsetComponentID  = 5
	OffsetMessageID    = 6 // 3 bytes for v2
)

// Common MAVLink message IDs we care about for telemetry
const (
	MsgIDHeartbeat         uint32 = 0
	MsgIDSysStatus         uint32 = 1
	MsgIDGPSRawInt         uint32 = 24
	MsgIDAttitude          uint32 = 30
	MsgIDGlobalPositionInt uint32 = 33
	MsgIDBatteryStatus     uint32 = 147
	MsgIDExtendedSysState  uint32 = 245
)

// MAVType represents the type of vehicle
type MAVType uint8

// MAVLink vehicle types.
const (
	MAVTypeGeneric        MAVType = 0
	MAVTypeFixedWing      MAVType = 1
	MAVTypeQuadrotor      MAVType = 2
	MAVTypeHexarotor      MAVType = 13
	MAVTypeOctorotor      MAVType = 14
	MAVTypeGroundRover    MAVType = 10
	MAVTypeSubmarine      MAVType = 12
)

func (t MAVType) String() string {
	switch t {
	case MAVTypeGeneric:
		return "generic"
	case MAVTypeFixedWing:
		return "fixed_wing"
	case MAVTypeQuadrotor:
		return "quadrotor"
	case MAVTypeHexarotor:
		return "hexarotor"
	case MAVTypeOctorotor:
		return "octorotor"
	case MAVTypeGroundRover:
		return "ground_rover"
	case MAVTypeSubmarine:
		return "submarine"
	default:
		return "unknown"
	}
}

// MAVState represents the flight state of the vehicle
type MAVState uint8

// MAVLink flight states.
const (
	MAVStateUninit      MAVState = 0
	MAVStateBoot        MAVState = 1
	MAVStateCalibrating MAVState = 2
	MAVStateStandby     MAVState = 3
	MAVStateActive      MAVState = 4
	MAVStateCritical    MAVState = 5
	MAVStateEmergency   MAVState = 6
	MAVStatePoweroff    MAVState = 7
)

func (s MAVState) String() string {
	switch s {
	case MAVStateUninit:
		return "uninitialized"
	case MAVStateBoot:
		return "boot"
	case MAVStateCalibrating:
		return "calibrating"
	case MAVStateStandby:
		return "standby"
	case MAVStateActive:
		return "active"
	case MAVStateCritical:
		return "critical"
	case MAVStateEmergency:
		return "emergency"
	case MAVStatePoweroff:
		return "poweroff"
	default:
		return "unknown"
	}
}

// GPSFixType represents the GPS fix quality
type GPSFixType uint8

// GPS fix quality levels.
const (
	GPSFixNone   GPSFixType = 0
	GPSFix2D     GPSFixType = 2
	GPSFix3D     GPSFixType = 3
	GPSFixDGPS   GPSFixType = 4
	GPSFixRTK    GPSFixType = 5
)

func (f GPSFixType) String() string {
	switch f {
	case GPSFixNone:
		return "no_fix"
	case GPSFix2D:
		return "2d_fix"
	case GPSFix3D:
		return "3d_fix"
	case GPSFixDGPS:
		return "dgps"
	case GPSFixRTK:
		return "rtk"
	default:
		return "unknown"
	}
}

// DroneID uniquely identifies a drone in the system
type DroneID struct {
	SystemID    uint8
	ComponentID uint8
}

// TelemetryEvent represents a parsed telemetry update from a drone.
// This is the canonical event type that flows through the system.
type TelemetryEvent struct {
	DroneID    DroneID
	MessageID  uint32
	Timestamp  time.Time
	SourceAddr string // UDP source address for debugging
	Payload    any    // Type-specific payload (Heartbeat, GPS, Battery, etc.)
}

// Heartbeat contains heartbeat message data
type Heartbeat struct {
	Type           MAVType
	Autopilot      uint8
	BaseMode       uint8
	CustomMode     uint32
	SystemStatus   MAVState
	MavlinkVersion uint8
	Armed          bool
}

// GPSPosition contains GPS position data
type GPSPosition struct {
	FixType       GPSFixType
	Latitude      float64 // Degrees
	Longitude     float64 // Degrees
	Altitude      float64 // Meters (MSL)
	RelAltitude   float64 // Meters (relative to home)
	GroundSpeed   float64 // m/s
	Heading       float64 // Degrees (0-360)
	SatelliteCount uint8
	HDOP          float64
	VDOP          float64
}

// BatteryStatus contains battery telemetry
type BatteryStatus struct {
	VoltageTotal   float64 // Volts
	CurrentBattery float64 // Amps
	Remaining      int8    // Percentage (0-100, -1 if unknown)
	TimeRemaining  int32   // Seconds remaining (-1 if unknown)
}

// Attitude contains vehicle attitude data
type Attitude struct {
	Roll       float64 // Radians
	Pitch      float64 // Radians
	Yaw        float64 // Radians
	RollSpeed  float64 // rad/s
	PitchSpeed float64 // rad/s
	YawSpeed   float64 // rad/s
}
