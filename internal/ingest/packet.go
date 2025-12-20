// Package ingest provides high-throughput UDP ingestion for MAVLink telemetry.
package ingest

import (
	"sync"
)

// Packet represents a raw UDP packet received from a drone.
// Packets are pooled to reduce GC pressure during high-throughput ingestion.
type Packet struct {
	Data       []byte // Raw packet data (up to ReadBufferSize)
	Length     int    // Actual bytes received
	SourceAddr string // Source address (IP:Port)
}

// Reset clears the packet for reuse.
func (p *Packet) Reset() {
	p.Length = 0
	p.SourceAddr = ""
	// Note: We don't zero the Data slice to avoid allocation
}

// Bytes returns the valid portion of the packet data.
func (p *Packet) Bytes() []byte {
	return p.Data[:p.Length]
}

// PacketPool provides efficient packet buffer recycling.
type PacketPool struct {
	pool       sync.Pool
	bufferSize int
}

// NewPacketPool creates a packet pool with the specified buffer size.
func NewPacketPool(bufferSize int) *PacketPool {
	return &PacketPool{
		bufferSize: bufferSize,
		pool: sync.Pool{
			New: func() any {
				return &Packet{
					Data: make([]byte, bufferSize),
				}
			},
		},
	}
}

// Get retrieves a packet from the pool.
func (pp *PacketPool) Get() *Packet {
	pkt := pp.pool.Get().(*Packet)
	pkt.Reset()
	return pkt
}

// Put returns a packet to the pool.
func (pp *PacketPool) Put(pkt *Packet) {
	if pkt == nil {
		return
	}
	// Only return packets with the expected buffer size
	if cap(pkt.Data) == pp.bufferSize {
		pp.pool.Put(pkt)
	}
}
