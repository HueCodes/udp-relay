// This file provides a command to generate a sample capture file
// from simulated MAVLink traffic.
//
// Usage: go run ./cmd/replay -generate -out testdata/sample_flight.bin
package main

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"time"
)

// CRC seeds (same as simulator).
var crcSeeds = map[uint32]byte{
	0:   50,
	1:   124,
	24:  24,
	30:  39,
	33:  104,
	147: 154,
}

func generateCapture(path string, durationSec int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Simple drone state for generation
	lat := 37.7749
	lon := -122.4194
	alt := 10.0
	heading := 0.0
	battery := int8(100)
	battV := 16.8
	seq := uint8(0)
	armed := false

	tickRate := 10 // Hz
	totalTicks := durationSec * tickRate
	tickDelay := uint32(1_000_000 / tickRate) // microseconds between frames

	var header [6]byte

	for tick := 0; tick < totalTicks; tick++ {
		// Simple flight: arm at t=3s, climb to 50m, fly a circle, land
		t := float64(tick) / float64(tickRate)

		if t >= 3 && !armed {
			armed = true
		}

		if armed && alt < 50 && t < 10 {
			alt += 0.5
		} else if armed && t >= 10 && t < 50 {
			// Circle
			angle := (t - 10) * 0.05 * 2 * math.Pi
			lat = 37.7749 + 0.001*math.Cos(angle)
			lon = -122.4194 + 0.001*math.Sin(angle)
			heading = math.Mod(angle*180/math.Pi+90, 360)
		} else if armed && t >= 50 {
			alt -= 0.3
			if alt <= 10.5 {
				alt = 10
				armed = false
			}
		}

		// GPS noise
		lat += (rand.Float64() - 0.5) * 0.0000003
		lon += (rand.Float64() - 0.5) * 0.0000003

		// Battery drain
		if armed && tick%tickRate == 0 && battery > 0 {
			battery--
			battV -= 0.01
		}

		// Cycle through message types
		var frame []byte
		msgType := tick % 5

		switch msgType {
		case 0:
			frame = buildSimFrame(&seq, 0, buildHB(armed))
		case 1:
			frame = buildSimFrame(&seq, 33, buildGP(lat, lon, alt, heading))
		case 2:
			frame = buildSimFrame(&seq, 30, buildAtt(0.01, -0.02, heading*math.Pi/180))
		case 3:
			frame = buildSimFrame(&seq, 147, buildBatt(battV, battery))
		case 4:
			frame = buildSimFrame(&seq, 24, buildGPS(lat, lon, alt, heading))
		}

		// Write header: delay_us(4) + frame_len(2)
		binary.LittleEndian.PutUint32(header[0:4], tickDelay)
		binary.LittleEndian.PutUint16(header[4:6], uint16(len(frame)))
		f.Write(header[:])
		f.Write(frame)
	}

	return nil
}

func buildSimFrame(seq *uint8, msgID uint32, payload []byte) []byte {
	frameSize := 1 + 9 + len(payload) + 2
	buf := make([]byte, frameSize)
	buf[0] = 0xFD
	buf[1] = byte(len(payload))
	buf[4] = *seq
	buf[5] = 1 // system ID
	buf[6] = 1 // component ID
	buf[7] = byte(msgID)
	buf[8] = byte(msgID >> 8)
	buf[9] = byte(msgID >> 16)
	copy(buf[10:], payload)

	if seed, ok := crcSeeds[msgID]; ok {
		crc := crcCalc(buf[1:10+len(payload)], seed)
		binary.LittleEndian.PutUint16(buf[10+len(payload):], crc)
	}

	*seq++
	return buf
}

func buildHB(armed bool) []byte {
	p := make([]byte, 9)
	binary.LittleEndian.PutUint32(p[0:4], 4<<16|4<<24) // Auto/Mission
	p[4] = 2  // quadrotor
	p[5] = 12 // PX4
	base := uint8(0x01)
	if armed {
		base |= 0x80
	}
	p[6] = base
	p[7] = 4 // active
	p[8] = 3
	return p
}

func buildGP(lat, lon, alt, heading float64) []byte {
	p := make([]byte, 28)
	binary.LittleEndian.PutUint32(p[0:4], uint32(time.Now().UnixMilli()))
	binary.LittleEndian.PutUint32(p[4:8], uint32(int32(lat*1e7)))
	binary.LittleEndian.PutUint32(p[8:12], uint32(int32(lon*1e7)))
	binary.LittleEndian.PutUint32(p[12:16], uint32(int32(alt*1000)))
	binary.LittleEndian.PutUint32(p[16:20], uint32(int32((alt-10)*1000)))
	binary.LittleEndian.PutUint16(p[26:28], uint16(heading*100))
	return p
}

func buildAtt(roll, pitch, yaw float64) []byte {
	p := make([]byte, 28)
	binary.LittleEndian.PutUint32(p[0:4], uint32(time.Now().UnixMilli()))
	binary.LittleEndian.PutUint32(p[4:8], math.Float32bits(float32(roll)))
	binary.LittleEndian.PutUint32(p[8:12], math.Float32bits(float32(pitch)))
	binary.LittleEndian.PutUint32(p[12:16], math.Float32bits(float32(yaw)))
	return p
}

func buildBatt(voltage float64, remaining int8) []byte {
	p := make([]byte, 36)
	cellV := uint16(voltage / 4 * 1000)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(p[10+i*2:], cellV)
	}
	for i := 4; i < 10; i++ {
		binary.LittleEndian.PutUint16(p[10+i*2:], 0xFFFF)
	}
	binary.LittleEndian.PutUint16(p[30:32], uint16(1500))
	p[35] = byte(remaining)
	return p
}

func buildGPS(lat, lon, alt, heading float64) []byte {
	p := make([]byte, 30)
	p[7] = 3 // fix type
	binary.LittleEndian.PutUint32(p[8:12], uint32(int32(lat*1e7)))
	binary.LittleEndian.PutUint32(p[12:16], uint32(int32(lon*1e7)))
	binary.LittleEndian.PutUint32(p[16:20], uint32(int32(alt*1000)))
	binary.LittleEndian.PutUint16(p[20:22], 120)
	binary.LittleEndian.PutUint16(p[22:24], 150)
	binary.LittleEndian.PutUint16(p[24:26], 500)
	binary.LittleEndian.PutUint16(p[26:28], uint16(heading*100))
	p[29] = 12
	return p
}

func crcAcc(b byte, crc uint16) uint16 {
	tmp := uint16(b) ^ (crc & 0xFF)
	tmp ^= (tmp << 4) & 0xFF
	return (crc >> 8) ^ (tmp << 8) ^ (tmp << 3) ^ (tmp >> 4)
}

func crcCalc(buf []byte, seed byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range buf {
		crc = crcAcc(b, crc)
	}
	crc = crcAcc(seed, crc)
	return crc
}
