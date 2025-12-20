package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/mavlink"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// UDPListener handles high-throughput UDP packet ingestion.
// It uses a worker pool pattern to process packets concurrently,
// ensuring that one misbehaving drone cannot block the pipeline.
type UDPListener struct {
	cfg        config.UDPConfig
	workerCfg  config.WorkerConfig
	conn       *net.UDPConn
	packetPool *PacketPool
	workerPool *WorkerPool

	// Output channel for parsed telemetry events
	output chan<- *protocol.TelemetryEvent

	// Metrics
	packetsReceived atomic.Uint64
	packetsDropped  atomic.Uint64
	parseErrors     atomic.Uint64

	logger *slog.Logger
	wg     sync.WaitGroup
}

// NewUDPListener creates a new UDP listener with the specified configuration.
func NewUDPListener(
	cfg config.UDPConfig,
	workerCfg config.WorkerConfig,
	output chan<- *protocol.TelemetryEvent,
	logger *slog.Logger,
) *UDPListener {
	return &UDPListener{
		cfg:        cfg,
		workerCfg:  workerCfg,
		packetPool: NewPacketPool(cfg.ReadBufferSize),
		output:     output,
		logger:     logger.With("component", "udp_listener"),
	}
}

// Start begins listening for UDP packets.
// It blocks until the context is cancelled or an unrecoverable error occurs.
func (l *UDPListener) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", l.cfg.BindAddress)
	if err != nil {
		return fmt.Errorf("resolve UDP address: %w", err)
	}

	l.conn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}

	// Set socket buffer size for high throughput
	if err := l.conn.SetReadBuffer(l.cfg.SocketBufferSize); err != nil {
		l.logger.Warn("failed to set socket buffer size",
			"requested", l.cfg.SocketBufferSize,
			"error", err)
	}

	l.logger.Info("UDP listener started",
		"address", l.cfg.BindAddress,
		"socket_buffer", l.cfg.SocketBufferSize)

	// Create the internal packet channel for the worker pool
	packetChan := make(chan *Packet, l.cfg.PacketQueueSize)

	// Start the worker pool
	l.workerPool = NewWorkerPool(
		l.workerCfg.PoolSize,
		packetChan,
		l.output,
		l.packetPool,
		l.logger,
	)
	l.workerPool.Start(ctx)

	// Start the receiver loop
	l.wg.Add(1)
	go l.receiveLoop(ctx, packetChan)

	// Wait for shutdown
	<-ctx.Done()

	// Graceful shutdown
	l.logger.Info("shutting down UDP listener")
	l.conn.Close()
	close(packetChan)
	l.wg.Wait()
	l.workerPool.Wait()

	l.logger.Info("UDP listener stopped",
		"packets_received", l.packetsReceived.Load(),
		"packets_dropped", l.packetsDropped.Load(),
		"parse_errors", l.parseErrors.Load())

	return nil
}

// receiveLoop continuously reads packets from the UDP socket.
func (l *UDPListener) receiveLoop(ctx context.Context, packetChan chan<- *Packet) {
	defer l.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Get a packet buffer from the pool
		pkt := l.packetPool.Get()

		// Set a short read deadline to allow checking for context cancellation
		l.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, remoteAddr, err := l.conn.ReadFromUDP(pkt.Data)
		if err != nil {
			// Check if it's a timeout (expected for periodic context checks)
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				l.packetPool.Put(pkt)
				continue
			}

			// Check if we're shutting down
			select {
			case <-ctx.Done():
				l.packetPool.Put(pkt)
				return
			default:
			}

			l.logger.Warn("UDP read error", "error", err)
			l.packetPool.Put(pkt)
			continue
		}

		pkt.Length = n
		pkt.SourceAddr = remoteAddr.String()
		l.packetsReceived.Add(1)

		// Non-blocking send to worker pool
		select {
		case packetChan <- pkt:
			// Successfully queued
		default:
			// Queue full - drop packet and return buffer to pool
			l.packetsDropped.Add(1)
			l.packetPool.Put(pkt)

			// Log warning periodically (not every dropped packet)
			if l.packetsDropped.Load()%1000 == 0 {
				l.logger.Warn("packet queue full, dropping packets",
					"total_dropped", l.packetsDropped.Load())
			}
		}
	}
}

// Stats returns current listener statistics.
func (l *UDPListener) Stats() ListenerStats {
	return ListenerStats{
		PacketsReceived: l.packetsReceived.Load(),
		PacketsDropped:  l.packetsDropped.Load(),
		ParseErrors:     l.parseErrors.Load(),
	}
}

// ListenerStats contains UDP listener statistics.
type ListenerStats struct {
	PacketsReceived uint64
	PacketsDropped  uint64
	ParseErrors     uint64
}

// WorkerPool processes packets concurrently using a fixed pool of workers.
// This prevents a single misbehaving drone (sending malformed packets)
// from blocking the entire ingest pipeline.
type WorkerPool struct {
	numWorkers int
	input      <-chan *Packet
	output     chan<- *protocol.TelemetryEvent
	packetPool *PacketPool
	decoder    *mavlink.Decoder
	logger     *slog.Logger

	// Rate limiters per system ID
	rateLimiters sync.Map // map[uint8]*rateLimiter

	wg sync.WaitGroup

	// Metrics
	processed atomic.Uint64
	errors    atomic.Uint64
}

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(
	numWorkers int,
	input <-chan *Packet,
	output chan<- *protocol.TelemetryEvent,
	packetPool *PacketPool,
	logger *slog.Logger,
) *WorkerPool {
	return &WorkerPool{
		numWorkers: numWorkers,
		input:      input,
		output:     output,
		packetPool: packetPool,
		decoder:    mavlink.NewDecoder(),
		logger:     logger.With("component", "worker_pool"),
	}
}

// Start launches the worker goroutines.
func (wp *WorkerPool) Start(ctx context.Context) {
	for i := 0; i < wp.numWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker(ctx, i)
	}
	wp.logger.Info("worker pool started", "workers", wp.numWorkers)
}

// Wait blocks until all workers have finished.
func (wp *WorkerPool) Wait() {
	wp.wg.Wait()
}

// worker processes packets from the input channel.
func (wp *WorkerPool) worker(ctx context.Context, id int) {
	defer wp.wg.Done()

	logger := wp.logger.With("worker_id", id)
	logger.Debug("worker started")

	for {
		select {
		case <-ctx.Done():
			logger.Debug("worker stopped")
			return

		case pkt, ok := <-wp.input:
			if !ok {
				logger.Debug("worker stopped (channel closed)")
				return
			}

			wp.processPacket(pkt)
		}
	}
}

// processPacket handles a single packet.
func (wp *WorkerPool) processPacket(pkt *Packet) {
	defer wp.packetPool.Put(pkt)

	// Parse the MAVLink frame
	event, err := wp.decoder.DecodePacket(pkt.Bytes(), pkt.SourceAddr)
	if err != nil {
		wp.errors.Add(1)
		// Don't log every parse error - could be noise from other protocols
		if wp.errors.Load()%100 == 0 {
			wp.logger.Debug("parse error",
				"error", err,
				"source", pkt.SourceAddr,
				"total_errors", wp.errors.Load())
		}
		return
	}

	// Check rate limit for this system ID
	if !wp.checkRateLimit(event.DroneID.SystemID) {
		// Rate limited - silently drop
		return
	}

	wp.processed.Add(1)

	// Send to output channel (non-blocking)
	select {
	case wp.output <- event:
		// Successfully sent
	default:
		// Output channel full - this shouldn't happen often
		wp.logger.Warn("output channel full, dropping event",
			"system_id", event.DroneID.SystemID)
	}
}

// rateLimiter tracks message rates per drone.
type rateLimiter struct {
	count     atomic.Int64
	resetTime atomic.Int64 // Unix timestamp in seconds
}

// checkRateLimit returns true if the message should be processed.
func (wp *WorkerPool) checkRateLimit(systemID uint8) bool {
	const maxMessagesPerSecond = 200 // Generous limit for telemetry

	now := time.Now().Unix()

	// Get or create rate limiter for this system ID
	limiterI, _ := wp.rateLimiters.LoadOrStore(systemID, &rateLimiter{})
	limiter := limiterI.(*rateLimiter)

	// Check if we need to reset the window
	resetTime := limiter.resetTime.Load()
	if now > resetTime {
		// Try to reset (atomic compare-and-swap)
		if limiter.resetTime.CompareAndSwap(resetTime, now+1) {
			limiter.count.Store(0)
		}
	}

	// Increment and check
	count := limiter.count.Add(1)
	return count <= maxMessagesPerSecond
}

// Stats returns worker pool statistics.
func (wp *WorkerPool) Stats() WorkerStats {
	return WorkerStats{
		Processed: wp.processed.Load(),
		Errors:    wp.errors.Load(),
	}
}

// WorkerStats contains worker pool statistics.
type WorkerStats struct {
	Processed uint64
	Errors    uint64
}
