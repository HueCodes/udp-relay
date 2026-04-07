package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
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
	droneCfg   config.DroneConfig
	conn       *net.UDPConn
	packetPool *PacketPool
	workerPool *WorkerPool

	// Output channel for parsed telemetry events
	output chan<- *protocol.TelemetryEvent

	// Source IP whitelist (nil = accept all)
	allowedNets []*net.IPNet

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
	droneCfg config.DroneConfig,
	output chan<- *protocol.TelemetryEvent,
	logger *slog.Logger,
) *UDPListener {
	l := &UDPListener{
		cfg:        cfg,
		workerCfg:  workerCfg,
		droneCfg:   droneCfg,
		packetPool: NewPacketPool(cfg.ReadBufferSize),
		output:     output,
		logger:     logger.With("component", "udp_listener"),
	}

	// Parse CIDR whitelist
	for _, cidr := range cfg.AllowedCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			logger.Warn("invalid CIDR in whitelist, skipping", "cidr", cidr, "error", err)
			continue
		}
		l.allowedNets = append(l.allowedNets, ipNet)
	}
	if len(l.allowedNets) > 0 {
		logger.Info("source IP whitelist enabled", "cidrs", len(l.allowedNets))
	}

	return l
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
		l.droneCfg.MaxMessagesPerSecond,
		l.droneCfg.RateLimitBurst,
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
	_ = l.conn.Close()
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
		_ = l.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

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

		// Check source IP whitelist
		if len(l.allowedNets) > 0 && !l.isAllowed(remoteAddr.IP) {
			l.packetsDropped.Add(1)
			l.packetPool.Put(pkt)
			continue
		}

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

// isAllowed checks if the source IP is in the whitelist.
func (l *UDPListener) isAllowed(ip net.IP) bool {
	for _, n := range l.allowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
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

	// Rate limiting config
	rateLimit int
	rateBurst int

	// Rate limiters per system ID
	rateLimiters sync.Map // map[uint8]*tokenBucket

	wg sync.WaitGroup

	// Metrics
	processed    atomic.Uint64
	errors       atomic.Uint64
	outputDrops  atomic.Uint64
}

// NewWorkerPool creates a new worker pool.
func NewWorkerPool(
	numWorkers int,
	input <-chan *Packet,
	output chan<- *protocol.TelemetryEvent,
	packetPool *PacketPool,
	rateLimit int,
	rateBurst int,
	logger *slog.Logger,
) *WorkerPool {
	return &WorkerPool{
		numWorkers: numWorkers,
		input:      input,
		output:     output,
		packetPool: packetPool,
		decoder:    mavlink.NewDecoder(),
		rateLimit:  rateLimit,
		rateBurst:  rateBurst,
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
		if !wp.runWorker(ctx, logger) {
			logger.Debug("worker stopped")
			return
		}
	}
}

func (wp *WorkerPool) runWorker(ctx context.Context, logger *slog.Logger) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("worker panicked, restarting",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			panicked = true
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return false

		case pkt, ok := <-wp.input:
			if !ok {
				return false
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
		dropped := wp.outputDrops.Add(1)
		if dropped%1000 == 1 {
			wp.logger.Warn("output channel full, dropping event",
				"system_id", event.DroneID.SystemID,
				"total_dropped", dropped)
		}
	}
}

// tokenBucket implements a simple token bucket rate limiter.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	rate     float64 // tokens per nanosecond
	lastTime int64   // UnixNano
}

func newTokenBucket(perSecond int, burst int) *tokenBucket {
	max := float64(perSecond + burst)
	return &tokenBucket{
		tokens:   max,
		max:      max,
		rate:     float64(perSecond) / 1e9,
		lastTime: time.Now().UnixNano(),
	}
}

// checkRateLimit returns true if the message should be processed.
func (wp *WorkerPool) checkRateLimit(systemID uint8) bool {
	limiterI, _ := wp.rateLimiters.LoadOrStore(systemID,
		newTokenBucket(wp.rateLimit, wp.rateBurst))
	bucket := limiterI.(*tokenBucket)

	now := time.Now().UnixNano()
	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	elapsed := now - bucket.lastTime
	bucket.lastTime = now

	// Refill tokens
	bucket.tokens += float64(elapsed) * bucket.rate
	if bucket.tokens > bucket.max {
		bucket.tokens = bucket.max
	}

	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
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
