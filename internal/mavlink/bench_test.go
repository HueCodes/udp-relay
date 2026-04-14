package mavlink

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/pkg/protocol"
)

// BenchmarkFullPipeline measures end-to-end: raw bytes -> parse -> decode -> JSON output.
func BenchmarkFullPipeline(b *testing.B) {
	decoder := NewDecoder()

	payload := makeGlobalPositionPayload(
		uint32(time.Now().UnixMilli()),
		int32(37.7749*1e7), int32(-122.4194*1e7),
		50000, 40000,
		500, 300, 0,
		uint16(270*100),
	)
	frame := buildBenchFrame(b, 33, 1, 1, payload)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		event, err := decoder.DecodePacket(frame, "10.0.0.1:14550")
		if err != nil {
			b.Fatal(err)
		}
		gps := event.Payload.(*protocol.GPSPosition)
		summary := struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
			Alt float64 `json:"alt"`
		}{gps.Latitude, gps.Longitude, gps.Altitude}
		_, err = json.Marshal(summary)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCRCThroughput measures raw CRC computation on max-size buffers.
func BenchmarkCRCThroughput(b *testing.B) {
	data := make([]byte, 255)
	for i := range data {
		data[i] = byte(i)
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		crcCalculate(data, 50)
	}
}

// BenchmarkParseAllMessageTypes measures parsing each supported message type.
func BenchmarkParseAllMessageTypes(b *testing.B) {
	types := []struct {
		name    string
		msgID   uint32
		payload []byte
	}{
		{"Heartbeat", 0, makeHeartbeatPayload(0, protocol.MAVTypeQuadrotor, 12, 0x81, protocol.MAVStateActive, 3)},
		{"Attitude", 30, makeAttitudePayload(1000, 0.1, -0.05, 1.5, 0.01, 0.005, 0.02)},
		{"GlobalPosition", 33, makeGlobalPositionPayload(1000, int32(37.7749*1e7), int32(-122.4194*1e7), 50000, 40000, 500, 300, 0, 27000)},
		{"GPSRaw", 24, makeGPSRawPayload(0, 3, int32(37.7749*1e7), int32(-122.4194*1e7), 50000, 120, 150, 500, 27000, 12)},
		{"Battery", 147, makeBatteryPayload([10]uint16{4200, 4200, 4200, 4200, 0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF}, 1500, 75)},
	}

	for _, tt := range types {
		frame := buildBenchFrame(b, tt.msgID, 1, 1, tt.payload)
		decoder := NewDecoder()

		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := decoder.DecodePacket(frame, "10.0.0.1:14550")
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
