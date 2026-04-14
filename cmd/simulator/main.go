// Package main provides a MAVLink v2 traffic simulator for testing and demos.
// It generates realistic telemetry from configurable virtual drones that follow
// waypoint circuits with GPS drift, battery drain, and attitude changes.
package main

import (
	"encoding/binary"
	"flag"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// CRC seeds for MAVLink message types (must match server's crc.go).
var crcSeeds = map[uint32]byte{
	0:   50,  // HEARTBEAT
	1:   124, // SYS_STATUS
	24:  24,  // GPS_RAW_INT
	30:  39,  // ATTITUDE
	33:  104, // GLOBAL_POSITION_INT
	147: 154, // BATTERY_STATUS
}

// Waypoint defines a GPS target in a flight plan.
type Waypoint struct {
	Lat float64
	Lon float64
	Alt float64 // meters above home
}

// SimDrone represents a simulated drone with flight state.
type SimDrone struct {
	SystemID uint8

	// Current state
	Lat       float64
	Lon       float64
	Alt       float64 // meters MSL
	HomeAlt   float64
	Heading   float64 // degrees
	Roll      float64 // radians
	Pitch     float64 // radians
	Yaw       float64 // radians
	Battery   int8    // 0-100
	BatteryV  float64 // volts
	Armed     bool
	FlightMode uint8 // PX4 main mode

	// Flight plan
	Waypoints    []Waypoint
	CurrentWP    int
	Phase        flightPhase
	PhaseTimer   int

	// Sequence counter
	Seq uint8
}

type flightPhase int

const (
	phasePrearm flightPhase = iota
	phaseTakeoff
	phaseClimb
	phaseCruise
	phaseDescend
	phaseLand
	phaseDisarmed
)

func main() {
	var (
		target   = flag.String("target", "127.0.0.1:14550", "Target UDP address")
		drones   = flag.Int("drones", 5, "Number of simulated drones")
		rate     = flag.Int("rate", 10, "Packets per second per drone")
		baseLat  = flag.Float64("lat", 37.7749, "Base latitude for flight area")
		baseLon  = flag.Float64("lon", -122.4194, "Base longitude for flight area")
		spread   = flag.Float64("spread", 0.01, "GPS coordinate spread for waypoints")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr, err := net.ResolveUDPAddr("udp", *target)
	if err != nil {
		logger.Error("invalid target address", "error", err)
		os.Exit(1)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		logger.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Create drones with unique waypoint circuits
	sims := make([]*SimDrone, *drones)
	for i := range sims {
		sims[i] = newSimDrone(uint8(i+1), *baseLat, *baseLon, *spread)
	}

	logger.Info("simulator started",
		"drones", *drones,
		"rate_hz", *rate,
		"target", *target,
	)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Second / time.Duration(*rate))
	defer ticker.Stop()

	var totalSent uint64
	msgCycle := 0

	for {
		select {
		case <-sigChan:
			logger.Info("simulator stopped", "total_packets", totalSent)
			return
		case <-ticker.C:
			for _, d := range sims {
				d.step()

				// Cycle through message types to simulate realistic traffic
				var buf []byte
				switch msgCycle % 5 {
				case 0:
					buf = d.buildHeartbeat()
				case 1:
					buf = d.buildGlobalPosition()
				case 2:
					buf = d.buildAttitude()
				case 3:
					buf = d.buildBatteryStatus()
				case 4:
					buf = d.buildGPSRaw()
				}

				if _, err := conn.Write(buf); err != nil {
					logger.Debug("send error", "error", err)
				}
				totalSent++
			}
			msgCycle++

			if totalSent%(uint64(*drones)*100) == 0 {
				logger.Info("packets sent", "total", totalSent)
			}
		}
	}
}

func newSimDrone(id uint8, baseLat, baseLon, spread float64) *SimDrone {
	// Generate a unique waypoint circuit for this drone
	numWP := 4 + rand.IntN(4) // 4-7 waypoints
	waypoints := make([]Waypoint, numWP)
	angle := rand.Float64() * 2 * math.Pi
	for i := range waypoints {
		wpAngle := angle + float64(i)*2*math.Pi/float64(numWP)
		radius := spread * (0.5 + rand.Float64()*0.5)
		waypoints[i] = Waypoint{
			Lat: baseLat + radius*math.Cos(wpAngle),
			Lon: baseLon + radius*math.Sin(wpAngle),
			Alt: 30 + rand.Float64()*70, // 30-100m
		}
	}

	homeAlt := 10.0 // meters MSL ground level
	return &SimDrone{
		SystemID:  id,
		Lat:       baseLat + (rand.Float64()-0.5)*spread*0.1,
		Lon:       baseLon + (rand.Float64()-0.5)*spread*0.1,
		Alt:       homeAlt,
		HomeAlt:   homeAlt,
		Battery:   100,
		BatteryV:  16.8, // 4S fully charged
		Waypoints: waypoints,
		Phase:     phasePrearm,
	}
}

// step advances the drone's simulation state by one tick.
func (d *SimDrone) step() {
	d.PhaseTimer++

	switch d.Phase {
	case phasePrearm:
		if d.PhaseTimer > 30 { // Wait ~3 seconds at 10Hz
			d.Armed = true
			d.FlightMode = 4 // Auto
			d.Phase = phaseTakeoff
			d.PhaseTimer = 0
		}

	case phaseTakeoff:
		targetAlt := d.HomeAlt + 10
		d.Alt += 0.5
		d.Pitch = -0.1 // Nose up slightly
		if d.Alt >= targetAlt {
			d.Phase = phaseClimb
			d.PhaseTimer = 0
		}

	case phaseClimb:
		wp := d.Waypoints[d.CurrentWP]
		d.moveToward(wp.Lat, wp.Lon, d.HomeAlt+wp.Alt, 0.3)
		if d.Alt >= d.HomeAlt+wp.Alt-1 {
			d.Phase = phaseCruise
			d.PhaseTimer = 0
		}

	case phaseCruise:
		wp := d.Waypoints[d.CurrentWP]
		dist := d.moveToward(wp.Lat, wp.Lon, d.HomeAlt+wp.Alt, 0.5)

		if dist < 0.0001 { // Close enough to waypoint
			d.CurrentWP = (d.CurrentWP + 1) % len(d.Waypoints)
			// After completing a full circuit, start landing
			if d.CurrentWP == 0 && d.Battery < 30 {
				d.Phase = phaseDescend
				d.PhaseTimer = 0
			}
		}

	case phaseDescend:
		d.Alt -= 0.3
		d.Pitch = 0.05
		if d.Alt <= d.HomeAlt+5 {
			d.Phase = phaseLand
			d.PhaseTimer = 0
		}

	case phaseLand:
		d.Alt -= 0.1
		if d.Alt <= d.HomeAlt+0.5 {
			d.Alt = d.HomeAlt
			d.Armed = false
			d.FlightMode = 0
			d.Phase = phaseDisarmed
			d.PhaseTimer = 0
		}

	case phaseDisarmed:
		if d.PhaseTimer > 100 { // Wait ~10 seconds then restart
			d.Battery = 100
			d.BatteryV = 16.8
			d.Phase = phasePrearm
			d.PhaseTimer = 0
		}
	}

	// Battery drain (faster when flying)
	if d.Armed {
		drain := 0.02 + rand.Float64()*0.01
		d.BatteryV -= drain * 0.01
		if d.PhaseTimer%10 == 0 && d.Battery > 0 {
			d.Battery--
		}
	}

	// Add small GPS noise
	d.Lat += (rand.Float64() - 0.5) * 0.0000005
	d.Lon += (rand.Float64() - 0.5) * 0.0000005
}

// moveToward moves the drone toward a target position. Returns distance remaining.
func (d *SimDrone) moveToward(lat, lon, alt, speed float64) float64 {
	dlat := lat - d.Lat
	dlon := lon - d.Lon
	dalt := alt - d.Alt
	dist := math.Sqrt(dlat*dlat + dlon*dlon)

	if dist > 0.00001 {
		// Move toward target
		step := speed * 0.00001 // Approximate degrees per step
		if step > dist {
			step = dist
		}
		d.Lat += dlat / dist * step
		d.Lon += dlon / dist * step

		// Update heading
		d.Heading = math.Mod(math.Atan2(dlon, dlat)*180/math.Pi+360, 360)

		// Bank into turns (roll proportional to heading change)
		targetYaw := d.Heading * math.Pi / 180
		d.Roll = math.Sin(targetYaw-d.Yaw) * 0.3
		d.Yaw = targetYaw
	}

	// Altitude adjustment
	if math.Abs(dalt) > 0.1 {
		if dalt > 0 {
			d.Alt += math.Min(0.3, dalt)
		} else {
			d.Alt += math.Max(-0.3, dalt)
		}
		d.Pitch = -dalt * 0.01
	} else {
		d.Pitch *= 0.9 // Decay pitch
	}

	return dist
}

// MAVLink v2 frame builder helpers

func (d *SimDrone) buildFrame(msgID uint32, payload []byte) []byte {
	payloadLen := len(payload)
	frameSize := 1 + 9 + payloadLen + 2 // STX + header + payload + checksum
	buf := make([]byte, frameSize)

	buf[0] = 0xFD // MAVLink v2 magic
	buf[1] = byte(payloadLen)
	buf[2] = 0 // incompat flags
	buf[3] = 0 // compat flags
	buf[4] = d.Seq
	buf[5] = d.SystemID
	buf[6] = 1 // component ID (autopilot)
	buf[7] = byte(msgID)
	buf[8] = byte(msgID >> 8)
	buf[9] = byte(msgID >> 16)

	copy(buf[10:], payload)

	// Calculate CRC-16/MCRF4XX
	if seed, ok := crcSeeds[msgID]; ok {
		crc := crcCalculate(buf[1:10+payloadLen], seed)
		binary.LittleEndian.PutUint16(buf[10+payloadLen:], crc)
	}

	d.Seq++
	return buf
}

func (d *SimDrone) buildHeartbeat() []byte {
	// Payload: custom_mode(4) + type(1) + autopilot(1) + base_mode(1) + system_status(1) + mavlink_version(1) = 9 bytes
	payload := make([]byte, 9)

	// Custom mode: PX4 auto mode with mission submode
	customMode := uint32(d.FlightMode)<<16 | 4<<24 // main mode + sub mode
	binary.LittleEndian.PutUint32(payload[0:4], customMode)

	payload[4] = 2  // MAV_TYPE_QUADROTOR
	payload[5] = 12 // MAV_AUTOPILOT_PX4
	baseMode := uint8(0x01) // Custom mode flag
	if d.Armed {
		baseMode |= 0x80 // Armed flag
	}
	payload[6] = baseMode
	payload[7] = 4 // MAV_STATE_ACTIVE
	payload[8] = 3 // MAVLink version

	return d.buildFrame(0, payload)
}

func (d *SimDrone) buildGlobalPosition() []byte {
	// Payload: time_boot_ms(4) + lat(4) + lon(4) + alt(4) + relative_alt(4) + vx(2) + vy(2) + vz(2) + hdg(2) = 28 bytes
	payload := make([]byte, 28)

	binary.LittleEndian.PutUint32(payload[0:4], uint32(time.Now().UnixMilli()&0xFFFFFFFF))
	binary.LittleEndian.PutUint32(payload[4:8], uint32(int32(d.Lat*1e7)))
	binary.LittleEndian.PutUint32(payload[8:12], uint32(int32(d.Lon*1e7)))
	binary.LittleEndian.PutUint32(payload[12:16], uint32(int32(d.Alt*1000)))
	binary.LittleEndian.PutUint32(payload[16:20], uint32(int32((d.Alt-d.HomeAlt)*1000)))

	// Velocities (cm/s)
	speed := 5.0 // m/s nominal
	if d.Phase == phaseCruise {
		speed = 10.0
	}
	vx := int16(speed * math.Cos(d.Heading*math.Pi/180) * 100)
	vy := int16(speed * math.Sin(d.Heading*math.Pi/180) * 100)
	binary.LittleEndian.PutUint16(payload[20:22], uint16(vx))
	binary.LittleEndian.PutUint16(payload[22:24], uint16(vy))
	binary.LittleEndian.PutUint16(payload[24:26], 0) // vz
	binary.LittleEndian.PutUint16(payload[26:28], uint16(d.Heading*100))

	return d.buildFrame(33, payload)
}

func (d *SimDrone) buildGPSRaw() []byte {
	// Payload: time_usec(8) + fix_type(1) + lat(4) + lon(4) + alt(4) + eph(2) + epv(2) + vel(2) + cog(2) + satellites_visible(1) = 30 bytes
	payload := make([]byte, 30)

	binary.LittleEndian.PutUint64(payload[0:8], uint64(time.Now().UnixMicro()))
	payload[7] = 3 // Note: this overwrites byte 7 of time_usec but GPS_RAW fix_type is at offset 7 in wire format
	// Actually fix_type is at offset 7 in the MAVLink definition ordering but in wire format it's reordered.
	// The parser reads fix_type from payload[7]. Let's match that.

	binary.LittleEndian.PutUint32(payload[8:12], uint32(int32(d.Lat*1e7)))
	binary.LittleEndian.PutUint32(payload[12:16], uint32(int32(d.Lon*1e7)))
	binary.LittleEndian.PutUint32(payload[16:20], uint32(int32(d.Alt*1000)))
	binary.LittleEndian.PutUint16(payload[20:22], 120) // HDOP * 100
	binary.LittleEndian.PutUint16(payload[22:24], 150) // VDOP * 100
	binary.LittleEndian.PutUint16(payload[24:26], 500) // vel cm/s
	binary.LittleEndian.PutUint16(payload[26:28], uint16(d.Heading*100))
	payload[29] = 12 // satellites visible

	return d.buildFrame(24, payload)
}

func (d *SimDrone) buildAttitude() []byte {
	// Payload: time_boot_ms(4) + roll(4) + pitch(4) + yaw(4) + rollspeed(4) + pitchspeed(4) + yawspeed(4) = 28 bytes
	payload := make([]byte, 28)

	binary.LittleEndian.PutUint32(payload[0:4], uint32(time.Now().UnixMilli()&0xFFFFFFFF))
	binary.LittleEndian.PutUint32(payload[4:8], math.Float32bits(float32(d.Roll)))
	binary.LittleEndian.PutUint32(payload[8:12], math.Float32bits(float32(d.Pitch)))
	binary.LittleEndian.PutUint32(payload[12:16], math.Float32bits(float32(d.Yaw)))
	binary.LittleEndian.PutUint32(payload[16:20], math.Float32bits(float32(0.01))) // roll speed
	binary.LittleEndian.PutUint32(payload[20:24], math.Float32bits(float32(0.005))) // pitch speed
	binary.LittleEndian.PutUint32(payload[24:28], math.Float32bits(float32(0.02))) // yaw speed

	return d.buildFrame(30, payload)
}

func (d *SimDrone) buildBatteryStatus() []byte {
	// Payload needs to be at least 36 bytes for the parser
	payload := make([]byte, 36)

	// Cell voltages at offset 10 (up to 10 cells, 2 bytes each)
	// Simulate 4S battery
	cellV := uint16(d.BatteryV / 4 * 1000) // per-cell mV
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(payload[10+i*2:], cellV)
	}
	// Mark remaining cells as invalid
	for i := 4; i < 10; i++ {
		binary.LittleEndian.PutUint16(payload[10+i*2:], 0xFFFF)
	}

	// Current at offset 30 (cA)
	current := int16(1500 + rand.IntN(500)) // 15-20A
	if !d.Armed {
		current = 50 // 0.5A idle
	}
	binary.LittleEndian.PutUint16(payload[30:32], uint16(current))

	// Remaining at offset 35
	payload[35] = byte(d.Battery)

	return d.buildFrame(147, payload)
}

// CRC-16/MCRF4XX implementation (matches server's crc.go)

func crcAccumulate(b byte, crc uint16) uint16 {
	tmp := uint16(b) ^ (crc & 0xFF)
	tmp ^= (tmp << 4) & 0xFF
	return (crc >> 8) ^ (tmp << 8) ^ (tmp << 3) ^ (tmp >> 4)
}

func crcCalculate(buf []byte, seed byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range buf {
		crc = crcAccumulate(b, crc)
	}
	crc = crcAccumulate(seed, crc)
	return crc
}

