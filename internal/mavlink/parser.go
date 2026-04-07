// Package mavlink provides efficient parsing of MAVLink v2 frames.
// It is designed for high-throughput ingestion with minimal allocations.
package mavlink

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/hugh/go-drone-server/pkg/protocol"
)

// Parser errors
var (
	ErrInvalidMagic       = errors.New("mavlink: invalid start byte")
	ErrFrameTooShort      = errors.New("mavlink: frame too short")
	ErrPayloadTooLarge    = errors.New("mavlink: payload exceeds maximum size")
	ErrChecksumMismatch   = errors.New("mavlink: checksum validation failed")
	ErrUnsupportedVersion = errors.New("mavlink: unsupported protocol version")
	ErrInvalidSystemID    = errors.New("mavlink: system ID out of valid range (1-250)")
	ErrInvalidTelemetry   = errors.New("mavlink: telemetry values out of valid range")
)

// Frame represents a parsed MAVLink v2 frame header.
// The payload is not copied; it references the original buffer.
type Frame struct {
	PayloadLength  uint8
	IncompatFlags  uint8
	CompatFlags    uint8
	Sequence       uint8
	SystemID       uint8
	ComponentID    uint8
	MessageID      uint32 // 24-bit message ID
	Payload        []byte // Slice into original buffer (zero-copy)
	Checksum       uint16
}

// Parser provides thread-safe MAVLink frame parsing with buffer pooling.
type Parser struct {
	// Buffer pool to reduce allocations during high-throughput parsing
	bufferPool sync.Pool

	// Whether to validate CRC checksums
	ValidateCRC bool
}

// NewParser creates a new MAVLink parser with optimized buffer pooling.
// CRC validation is enabled by default.
func NewParser() *Parser {
	return &Parser{
		ValidateCRC: true,
		bufferPool: sync.Pool{
			New: func() any {
				buf := make([]byte, 0, 300)
				return &buf
			},
		},
	}
}

// ParseFrame extracts a MAVLink frame from raw bytes.
// It performs validation but does NOT copy the payload for efficiency.
// The caller must process or copy the frame before reusing the buffer.
//
// Returns the frame and the number of bytes consumed, or an error.
func (p *Parser) ParseFrame(data []byte) (*Frame, int, error) {
	if len(data) < 1 {
		return nil, 0, ErrFrameTooShort
	}

	// Check magic byte
	switch data[0] {
	case protocol.MagicV2:
		return p.parseV2Frame(data)
	case protocol.MagicV1:
		// V1 support could be added here, but we focus on v2 for PX4
		return nil, 0, ErrUnsupportedVersion
	default:
		return nil, 0, ErrInvalidMagic
	}
}

// parseV2Frame parses a MAVLink v2 frame.
func (p *Parser) parseV2Frame(data []byte) (*Frame, int, error) {
	// Minimum v2 frame: STX(1) + Header(9) + Checksum(2) = 12 bytes (no payload)
	const minFrameSize = 1 + 9 + 2

	if len(data) < minFrameSize {
		return nil, 0, ErrFrameTooShort
	}

	payloadLen := data[1]
	if payloadLen > protocol.MaxPayloadSize {
		return nil, 0, ErrPayloadTooLarge
	}

	// Total frame size: STX + Header(9) + Payload + Checksum(2) + optional signature(13)
	frameSize := 1 + 9 + int(payloadLen) + 2

	// Check for signature flag
	incompatFlags := data[2]
	if incompatFlags&0x01 != 0 {
		frameSize += 13 // Signature present
	}

	if len(data) < frameSize {
		return nil, 0, ErrFrameTooShort
	}

	// Extract 24-bit message ID (little-endian)
	msgID := uint32(data[7]) | uint32(data[8])<<8 | uint32(data[9])<<16

	frame := &Frame{
		PayloadLength: payloadLen,
		IncompatFlags: incompatFlags,
		CompatFlags:   data[3],
		Sequence:      data[4],
		SystemID:      data[5],
		ComponentID:   data[6],
		MessageID:     msgID,
	}

	// Zero-copy payload reference
	payloadStart := 10
	payloadEnd := payloadStart + int(payloadLen)
	frame.Payload = data[payloadStart:payloadEnd]

	// Extract checksum (little-endian, after payload)
	frame.Checksum = binary.LittleEndian.Uint16(data[payloadEnd : payloadEnd+2])

	// Validate CRC-16/MCRF4XX if enabled
	if p.ValidateCRC {
		if seed, ok := crcSeed[msgID]; ok {
			// CRC covers bytes 1..payloadEnd (header + payload, excluding STX)
			computed := crcCalculate(data[1:payloadEnd], seed)
			if computed != frame.Checksum {
				return nil, 0, ErrChecksumMismatch
			}
		}
		// Unknown message IDs: skip CRC check (no seed available)
	}

	return frame, frameSize, nil
}

// Decoder provides stateful decoding of MAVLink messages.
// It translates raw Frame payloads into typed Go structures.
type Decoder struct {
	parser *Parser
}

// NewDecoder creates a new MAVLink message decoder.
func NewDecoder() *Decoder {
	return &Decoder{
		parser: NewParser(),
	}
}

// NewDecoderWithCRC creates a decoder with configurable CRC validation.
func NewDecoderWithCRC(validateCRC bool) *Decoder {
	p := NewParser()
	p.ValidateCRC = validateCRC
	return &Decoder{parser: p}
}

// DecodePacket parses a raw packet and returns a TelemetryEvent.
// This is the main entry point for the ingest pipeline.
func (d *Decoder) DecodePacket(data []byte, sourceAddr string) (*protocol.TelemetryEvent, error) {
	frame, _, err := d.parser.ParseFrame(data)
	if err != nil {
		return nil, err
	}

	// Validate system ID range (1-250)
	if !ValidateSystemID(frame.SystemID) {
		return nil, ErrInvalidSystemID
	}

	event := &protocol.TelemetryEvent{
		DroneID: protocol.DroneID{
			SystemID:    frame.SystemID,
			ComponentID: frame.ComponentID,
		},
		MessageID:  frame.MessageID,
		Timestamp:  time.Now(),
		SourceAddr: sourceAddr,
	}

	// Decode and validate payload based on message type
	payload := d.decodePayload(frame)

	// Validate telemetry values
	switch p := payload.(type) {
	case *protocol.GPSPosition:
		if p != nil && !ValidateGPS(p) {
			return nil, ErrInvalidTelemetry
		}
	case *protocol.BatteryStatus:
		if p != nil && !ValidateBattery(p) {
			return nil, ErrInvalidTelemetry
		}
	}

	event.Payload = payload
	return event, nil
}

// decodePayload decodes the frame payload into a typed structure.
func (d *Decoder) decodePayload(frame *Frame) any {
	switch frame.MessageID {
	case protocol.MsgIDHeartbeat:
		return d.decodeHeartbeat(frame.Payload)
	case protocol.MsgIDGlobalPositionInt:
		return d.decodeGlobalPosition(frame.Payload)
	case protocol.MsgIDGPSRawInt:
		return d.decodeGPSRaw(frame.Payload)
	case protocol.MsgIDBatteryStatus:
		return d.decodeBatteryStatus(frame.Payload)
	case protocol.MsgIDAttitude:
		return d.decodeAttitude(frame.Payload)
	default:
		// Return raw payload for unsupported messages
		return frame.Payload
	}
}

// decodeHeartbeat decodes a HEARTBEAT message (ID 0).
// Payload: type(1) + autopilot(1) + base_mode(1) + custom_mode(4) + system_status(1) + mavlink_version(1) = 9 bytes
func (d *Decoder) decodeHeartbeat(payload []byte) *protocol.Heartbeat {
	if len(payload) < 9 {
		return nil
	}

	hb := &protocol.Heartbeat{
		Type:           protocol.MAVType(payload[4]),
		Autopilot:      payload[5],
		BaseMode:       payload[6],
		CustomMode:     binary.LittleEndian.Uint32(payload[0:4]),
		SystemStatus:   protocol.MAVState(payload[7]),
		MavlinkVersion: payload[8],
	}

	// Check armed flag (bit 7 of base_mode)
	hb.Armed = hb.BaseMode&0x80 != 0

	return hb
}

// decodeGlobalPosition decodes a GLOBAL_POSITION_INT message (ID 33).
// Payload: time_boot_ms(4) + lat(4) + lon(4) + alt(4) + relative_alt(4) + vx(2) + vy(2) + vz(2) + hdg(2) = 28 bytes
func (d *Decoder) decodeGlobalPosition(payload []byte) *protocol.GPSPosition {
	if len(payload) < 28 {
		return nil
	}

	lat := int32(binary.LittleEndian.Uint32(payload[4:8]))
	lon := int32(binary.LittleEndian.Uint32(payload[8:12]))
	alt := int32(binary.LittleEndian.Uint32(payload[12:16]))
	relAlt := int32(binary.LittleEndian.Uint32(payload[16:20]))
	vx := int16(binary.LittleEndian.Uint16(payload[20:22]))
	vy := int16(binary.LittleEndian.Uint16(payload[22:24]))
	hdg := binary.LittleEndian.Uint16(payload[26:28])

	// Calculate ground speed from vx, vy (cm/s to m/s)
	groundSpeed := (float64(int32(vx))*float64(int32(vx)) + float64(int32(vy))*float64(int32(vy))) / 10000.0

	return &protocol.GPSPosition{
		Latitude:    float64(lat) / 1e7,
		Longitude:   float64(lon) / 1e7,
		Altitude:    float64(alt) / 1000.0,
		RelAltitude: float64(relAlt) / 1000.0,
		GroundSpeed: groundSpeed,
		Heading:     float64(hdg) / 100.0,
	}
}

// decodeGPSRaw decodes a GPS_RAW_INT message (ID 24).
// Payload: time_usec(8) + fix_type(1) + lat(4) + lon(4) + alt(4) + eph(2) + epv(2) + vel(2) + cog(2) + satellites_visible(1)
func (d *Decoder) decodeGPSRaw(payload []byte) *protocol.GPSPosition {
	if len(payload) < 30 {
		return nil
	}

	lat := int32(binary.LittleEndian.Uint32(payload[8:12]))
	lon := int32(binary.LittleEndian.Uint32(payload[12:16]))
	alt := int32(binary.LittleEndian.Uint32(payload[16:20]))
	eph := binary.LittleEndian.Uint16(payload[20:22])
	epv := binary.LittleEndian.Uint16(payload[22:24])
	vel := binary.LittleEndian.Uint16(payload[24:26])
	cog := binary.LittleEndian.Uint16(payload[26:28])

	return &protocol.GPSPosition{
		FixType:        protocol.GPSFixType(payload[7]),
		Latitude:       float64(lat) / 1e7,
		Longitude:      float64(lon) / 1e7,
		Altitude:       float64(alt) / 1000.0,
		GroundSpeed:    float64(vel) / 100.0,
		Heading:        float64(cog) / 100.0,
		SatelliteCount: payload[29],
		HDOP:           float64(eph) / 100.0,
		VDOP:           float64(epv) / 100.0,
	}
}

// decodeBatteryStatus decodes a BATTERY_STATUS message (ID 147).
func (d *Decoder) decodeBatteryStatus(payload []byte) *protocol.BatteryStatus {
	if len(payload) < 36 {
		return nil
	}

	// voltages are in array starting at offset 10, each 2 bytes (up to 10 cells)
	// We'll sum the first few non-UINT16_MAX values
	var totalVoltage float64
	for i := 0; i < 10; i++ {
		offset := 10 + i*2
		if offset+2 > len(payload) {
			break
		}
		cellVoltage := binary.LittleEndian.Uint16(payload[offset : offset+2])
		if cellVoltage != 0xFFFF && cellVoltage != 0 {
			totalVoltage += float64(cellVoltage) / 1000.0
		}
	}

	current := int16(binary.LittleEndian.Uint16(payload[30:32]))
	remaining := int8(payload[35])

	return &protocol.BatteryStatus{
		VoltageTotal:   totalVoltage,
		CurrentBattery: float64(current) / 100.0, // cA to A
		Remaining:      remaining,
		TimeRemaining:  -1, // Not available in basic message
	}
}

// decodeAttitude decodes an ATTITUDE message (ID 30).
// Payload: time_boot_ms(4) + roll(4) + pitch(4) + yaw(4) + rollspeed(4) + pitchspeed(4) + yawspeed(4) = 28 bytes
func (d *Decoder) decodeAttitude(payload []byte) *protocol.Attitude {
	if len(payload) < 28 {
		return nil
	}

	return &protocol.Attitude{
		Roll:       float64(readFloat32LE(payload[4:8])),
		Pitch:      float64(readFloat32LE(payload[8:12])),
		Yaw:        float64(readFloat32LE(payload[12:16])),
		RollSpeed:  float64(readFloat32LE(payload[16:20])),
		PitchSpeed: float64(readFloat32LE(payload[20:24])),
		YawSpeed:   float64(readFloat32LE(payload[24:28])),
	}
}

// readFloat32LE reads a little-endian float32 from bytes.
func readFloat32LE(b []byte) float32 {
	bits := binary.LittleEndian.Uint32(b)
	return math.Float32frombits(bits)
}
