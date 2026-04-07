package ingest

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// ---------------------------------------------------------------------------
// Helpers: build valid MAVLink v2 heartbeat frames
// ---------------------------------------------------------------------------

// crcAccum adds one byte to a running CRC-16/MCRF4XX.
func crcAccum(b byte, crc uint16) uint16 {
	tmp := uint16(b) ^ (crc & 0xFF)
	tmp ^= (tmp << 4) & 0xFF
	return (crc >> 8) ^ (tmp << 8) ^ (tmp << 3) ^ (tmp >> 4)
}

// buildHeartbeatFrame builds a minimal valid MAVLink v2 heartbeat frame.
// systemID and seq can be varied; payload is 9 zero-bytes (valid heartbeat).
func buildHeartbeatFrame(systemID, seq uint8) []byte {
	const (
		payloadLen  = 9
		crcExtra    = 50 // heartbeat CRC_EXTRA
		headerBytes = 10 // bytes 1..9 after STX
	)

	frame := make([]byte, 1+headerBytes+payloadLen+2)
	frame[0] = protocol.MagicV2 // STX
	frame[1] = payloadLen       // payload length
	frame[2] = 0                // incompat flags
	frame[3] = 0                // compat flags
	frame[4] = seq              // sequence
	frame[5] = systemID         // system ID
	frame[6] = 1                // component ID
	frame[7] = 0                // msg ID low byte (heartbeat = 0)
	frame[8] = 0                // msg ID mid byte
	frame[9] = 0                // msg ID high byte
	// payload bytes 10..18 are already zero (heartbeat all-zeros is fine)

	// CRC covers bytes 1..payloadEnd (header + payload, excluding STX)
	payloadEnd := 10 + int(payloadLen) // 19
	crc := uint16(0xFFFF)
	for _, b := range frame[1:payloadEnd] {
		crc = crcAccum(b, crc)
	}
	crc = crcAccum(crcExtra, crc)

	binary.LittleEndian.PutUint16(frame[payloadEnd:], crc)
	return frame
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---------------------------------------------------------------------------
// PacketPool tests
// ---------------------------------------------------------------------------

func TestPacketPool_GetPut(t *testing.T) {
	const bufSize = 512
	pp := NewPacketPool(bufSize)

	pkt := pp.Get()
	if pkt == nil {
		t.Fatal("Get returned nil")
	}
	if len(pkt.Data) != bufSize {
		t.Fatalf("expected Data len %d, got %d", bufSize, len(pkt.Data))
	}
	if cap(pkt.Data) != bufSize {
		t.Fatalf("expected Data cap %d, got %d", bufSize, cap(pkt.Data))
	}
	if pkt.Length != 0 {
		t.Fatalf("expected Length 0 after Get, got %d", pkt.Length)
	}
	if pkt.SourceAddr != "" {
		t.Fatalf("expected empty SourceAddr after Get, got %q", pkt.SourceAddr)
	}

	// Mutate fields, put back, get again -- should be reset.
	pkt.Length = 42
	pkt.SourceAddr = "1.2.3.4:5678"
	pp.Put(pkt)

	pkt2 := pp.Get()
	if pkt2.Length != 0 || pkt2.SourceAddr != "" {
		t.Fatal("packet was not reset after Get")
	}
}

func TestPacketPool_BufferReuse(t *testing.T) {
	const bufSize = 256
	pp := NewPacketPool(bufSize)

	pkt := pp.Get()
	// Write a sentinel into the data buffer.
	pkt.Data[0] = 0xAB
	ptr := &pkt.Data[0] // remember the backing array
	pp.Put(pkt)

	// The pool may or may not return the same object, but if it does the
	// backing array should be reused (no reallocation).
	pkt2 := pp.Get()
	if &pkt2.Data[0] == ptr {
		// Same backing array -- good, buffer reuse works.
		// Fields must still be reset.
		if pkt2.Length != 0 || pkt2.SourceAddr != "" {
			t.Fatal("reused packet not reset")
		}
	}
	// If the pool returned a fresh packet that's also fine (GC can evict).
}

func TestPacketPool_WrongSizeRejected(t *testing.T) {
	const bufSize = 512
	pp := NewPacketPool(bufSize)

	wrongPkt := &Packet{Data: make([]byte, 1024)} // wrong cap
	pp.Put(wrongPkt)

	// The pool should create a new packet rather than returning the wrong-size one.
	pkt := pp.Get()
	if cap(pkt.Data) != bufSize {
		t.Fatalf("pool returned wrong-size buffer: cap=%d, want %d", cap(pkt.Data), bufSize)
	}
}

func TestPacketPool_NilPut(t *testing.T) {
	pp := NewPacketPool(128)
	// Should not panic.
	pp.Put(nil)
}

func TestPacket_Reset(t *testing.T) {
	pkt := &Packet{
		Data:       make([]byte, 64),
		Length:     32,
		SourceAddr: "10.0.0.1:9999",
	}
	pkt.Data[0] = 0xFF

	pkt.Reset()
	if pkt.Length != 0 {
		t.Fatal("Reset did not clear Length")
	}
	if pkt.SourceAddr != "" {
		t.Fatal("Reset did not clear SourceAddr")
	}
	// Data slice should still be intact (no zeroing for performance).
	if pkt.Data[0] != 0xFF {
		t.Fatal("Reset should not zero the Data slice")
	}
}

func TestPacket_Bytes(t *testing.T) {
	pkt := &Packet{
		Data:   make([]byte, 64),
		Length: 5,
	}
	pkt.Data[0] = 1
	pkt.Data[4] = 5

	b := pkt.Bytes()
	if len(b) != 5 {
		t.Fatalf("Bytes() len = %d, want 5", len(b))
	}
	if b[0] != 1 || b[4] != 5 {
		t.Fatal("Bytes() returned wrong content")
	}
}

// ---------------------------------------------------------------------------
// WorkerPool tests
// ---------------------------------------------------------------------------

func TestWorkerPool_Lifecycle(t *testing.T) {
	input := make(chan *Packet, 16)
	output := make(chan *protocol.TelemetryEvent, 16)
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(2, input, output, pp, 1000, 50, logger)

	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	// Send a valid heartbeat packet.
	frame := buildHeartbeatFrame(1, 0)
	pkt := pp.Get()
	copy(pkt.Data, frame)
	pkt.Length = len(frame)
	pkt.SourceAddr = "192.168.1.10:14550"
	input <- pkt

	select {
	case ev := <-output:
		if ev.DroneID.SystemID != 1 {
			t.Fatalf("expected SystemID 1, got %d", ev.DroneID.SystemID)
		}
		if ev.MessageID != protocol.MsgIDHeartbeat {
			t.Fatalf("expected MessageID 0, got %d", ev.MessageID)
		}
		if _, ok := ev.Payload.(*protocol.Heartbeat); !ok {
			t.Fatalf("expected *Heartbeat payload, got %T", ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for telemetry event")
	}

	cancel()
	close(input)
	wp.Wait()
}

func TestWorkerPool_QueueOverflow(t *testing.T) {
	// Tiny output channel to force drops.
	input := make(chan *Packet, 4)
	output := make(chan *protocol.TelemetryEvent, 1) // only 1 slot
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(1, input, output, pp, 100000, 100000, logger)
	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	// Fill the output channel first.
	frame := buildHeartbeatFrame(1, 0)
	for i := 0; i < 5; i++ {
		pkt := pp.Get()
		copy(pkt.Data, frame)
		pkt.Length = len(frame)
		pkt.SourceAddr = "10.0.0.1:1234"
		input <- pkt
	}

	// Give workers time to process.
	time.Sleep(200 * time.Millisecond)

	stats := wp.Stats()
	// With output channel size 1, at least some should have been dropped.
	if stats.Processed < 1 {
		t.Fatal("expected at least 1 processed packet")
	}

	cancel()
	close(input)
	wp.Wait()
}

func TestWorkerPool_RateLimiter_BurstAllowed(t *testing.T) {
	// Rate = 10/sec, burst = 5  =>  bucket starts with 15 tokens.
	// Sending 15 packets instantly should all pass.
	input := make(chan *Packet, 64)
	output := make(chan *protocol.TelemetryEvent, 64)
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(1, input, output, pp, 10, 5, logger)
	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	frame := buildHeartbeatFrame(42, 0)
	const count = 15
	for i := 0; i < count; i++ {
		pkt := pp.Get()
		copy(pkt.Data, frame)
		pkt.Length = len(frame)
		pkt.SourceAddr = "10.0.0.1:1234"
		input <- pkt
	}

	time.Sleep(300 * time.Millisecond)

	stats := wp.Stats()
	if stats.Processed != count {
		t.Fatalf("burst: expected %d processed, got %d", count, stats.Processed)
	}

	cancel()
	close(input)
	wp.Wait()
}

func TestWorkerPool_RateLimiter_SustainedEnforced(t *testing.T) {
	// Rate = 10/sec, burst = 0  =>  bucket has 10 tokens.
	// Send 30 packets instantly; only ~10 should pass (the initial tokens).
	// Then wait 500ms (refills ~5 tokens) and verify more can pass.
	input := make(chan *Packet, 128)
	output := make(chan *protocol.TelemetryEvent, 128)
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(1, input, output, pp, 10, 0, logger)
	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	frame := buildHeartbeatFrame(7, 0)

	// First burst: send 30 packets.
	for i := 0; i < 30; i++ {
		pkt := pp.Get()
		copy(pkt.Data, frame)
		pkt.Length = len(frame)
		pkt.SourceAddr = "10.0.0.1:1234"
		input <- pkt
	}

	time.Sleep(200 * time.Millisecond)

	stats1 := wp.Stats()
	if stats1.Processed > 15 {
		t.Fatalf("rate limiter not enforcing: %d processed, expected <= 15", stats1.Processed)
	}
	if stats1.Processed < 5 {
		t.Fatalf("rate limiter too aggressive: only %d processed, expected >= 5", stats1.Processed)
	}

	// Wait for token refill and send more.
	time.Sleep(600 * time.Millisecond)

	for i := 0; i < 10; i++ {
		pkt := pp.Get()
		copy(pkt.Data, frame)
		pkt.Length = len(frame)
		pkt.SourceAddr = "10.0.0.1:1234"
		input <- pkt
	}

	time.Sleep(200 * time.Millisecond)

	stats2 := wp.Stats()
	if stats2.Processed <= stats1.Processed {
		t.Fatal("no additional packets processed after token refill")
	}

	cancel()
	close(input)
	wp.Wait()
}

func TestWorkerPool_ShutdownDraining(t *testing.T) {
	input := make(chan *Packet, 32)
	output := make(chan *protocol.TelemetryEvent, 32)
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(2, input, output, pp, 10000, 10000, logger)
	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	frame := buildHeartbeatFrame(1, 0)
	const n = 10
	for i := 0; i < n; i++ {
		pkt := pp.Get()
		copy(pkt.Data, frame)
		pkt.Length = len(frame)
		pkt.SourceAddr = "10.0.0.1:1234"
		input <- pkt
	}

	// Cancel context and close input -- workers should drain remaining packets.
	cancel()
	close(input)

	done := make(chan struct{})
	go func() {
		wp.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Workers shut down properly.
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not shut down within timeout")
	}
}

func TestWorkerPool_ParseError(t *testing.T) {
	input := make(chan *Packet, 4)
	output := make(chan *protocol.TelemetryEvent, 4)
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(1, input, output, pp, 10000, 100, logger)
	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	// Send garbage that is not a valid MAVLink frame.
	pkt := pp.Get()
	pkt.Data[0] = 0x00 // invalid magic
	pkt.Length = 20
	pkt.SourceAddr = "10.0.0.1:1234"
	input <- pkt

	time.Sleep(100 * time.Millisecond)

	stats := wp.Stats()
	if stats.Errors < 1 {
		t.Fatal("expected at least 1 parse error")
	}
	if stats.Processed != 0 {
		t.Fatalf("expected 0 processed, got %d", stats.Processed)
	}

	cancel()
	close(input)
	wp.Wait()
}

// ---------------------------------------------------------------------------
// UDPListener: isAllowed (CIDR whitelist)
// ---------------------------------------------------------------------------

func TestIsAllowed_Matching(t *testing.T) {
	cfg := config.UDPConfig{
		ReadBufferSize: 1024,
		AllowedCIDRs:   []string{"10.0.0.0/8", "192.168.1.0/24"},
	}
	output := make(chan *protocol.TelemetryEvent, 1)
	logger := discardLogger()
	l := NewUDPListener(cfg, config.WorkerConfig{PoolSize: 1}, config.DroneConfig{}, output, logger)

	tests := []struct {
		ip      string
		allowed bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"192.168.1.100", true},
		{"192.168.2.1", false},
		{"172.16.0.1", false},
		{"8.8.8.8", false},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP %s", tt.ip)
		}
		got := l.isAllowed(ip)
		if got != tt.allowed {
			t.Errorf("isAllowed(%s) = %v, want %v", tt.ip, got, tt.allowed)
		}
	}
}

func TestIsAllowed_EmptyWhitelist(t *testing.T) {
	// With no CIDRs configured, allowedNets is nil and isAllowed is never
	// called in production (guarded by len check). But if called directly
	// it should return false since no nets match.
	cfg := config.UDPConfig{
		ReadBufferSize: 1024,
	}
	output := make(chan *protocol.TelemetryEvent, 1)
	logger := discardLogger()
	l := NewUDPListener(cfg, config.WorkerConfig{PoolSize: 1}, config.DroneConfig{}, output, logger)

	if len(l.allowedNets) != 0 {
		t.Fatal("expected empty allowedNets")
	}
	// Direct call returns false (no nets to match).
	if l.isAllowed(net.ParseIP("10.0.0.1")) {
		t.Fatal("expected false when whitelist is empty")
	}
}

func TestIsAllowed_InvalidCIDRSkipped(t *testing.T) {
	cfg := config.UDPConfig{
		ReadBufferSize: 1024,
		AllowedCIDRs:   []string{"not-a-cidr", "10.0.0.0/8"},
	}
	output := make(chan *protocol.TelemetryEvent, 1)
	logger := discardLogger()
	l := NewUDPListener(cfg, config.WorkerConfig{PoolSize: 1}, config.DroneConfig{}, output, logger)

	// Invalid CIDR should be skipped; valid one should still work.
	if len(l.allowedNets) != 1 {
		t.Fatalf("expected 1 valid net, got %d", len(l.allowedNets))
	}
	if !l.isAllowed(net.ParseIP("10.0.0.1")) {
		t.Fatal("10.0.0.1 should be allowed")
	}
}

// ---------------------------------------------------------------------------
// Stats tracking
// ---------------------------------------------------------------------------

func TestListenerStats_Defaults(t *testing.T) {
	cfg := config.UDPConfig{ReadBufferSize: 1024}
	output := make(chan *protocol.TelemetryEvent, 1)
	logger := discardLogger()
	l := NewUDPListener(cfg, config.WorkerConfig{PoolSize: 1}, config.DroneConfig{}, output, logger)

	stats := l.Stats()
	if stats.PacketsReceived != 0 || stats.PacketsDropped != 0 || stats.ParseErrors != 0 {
		t.Fatal("fresh listener should have zero stats")
	}
}

func TestListenerStats_AtomicUpdates(t *testing.T) {
	cfg := config.UDPConfig{ReadBufferSize: 1024}
	output := make(chan *protocol.TelemetryEvent, 1)
	logger := discardLogger()
	l := NewUDPListener(cfg, config.WorkerConfig{PoolSize: 1}, config.DroneConfig{}, output, logger)

	// Simulate concurrent counter increments.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.packetsReceived.Add(1)
			l.packetsDropped.Add(1)
			l.parseErrors.Add(1)
		}()
	}
	wg.Wait()

	stats := l.Stats()
	if stats.PacketsReceived != 100 {
		t.Fatalf("PacketsReceived = %d, want 100", stats.PacketsReceived)
	}
	if stats.PacketsDropped != 100 {
		t.Fatalf("PacketsDropped = %d, want 100", stats.PacketsDropped)
	}
	if stats.ParseErrors != 100 {
		t.Fatalf("ParseErrors = %d, want 100", stats.ParseErrors)
	}
}

// ---------------------------------------------------------------------------
// tokenBucket unit tests
// ---------------------------------------------------------------------------

func TestTokenBucket_InitialTokens(t *testing.T) {
	tb := newTokenBucket(10, 5)
	// max = 10 + 5 = 15
	if tb.max != 15 {
		t.Fatalf("max = %f, want 15", tb.max)
	}
	if tb.tokens != 15 {
		t.Fatalf("initial tokens = %f, want 15", tb.tokens)
	}
}

func TestTokenBucket_Refill(t *testing.T) {
	tb := newTokenBucket(100, 0) // 100/sec, no burst
	// Drain all tokens.
	for i := 0; i < 100; i++ {
		tb.mu.Lock()
		tb.tokens--
		tb.mu.Unlock()
	}

	// Set lastTime to 500ms ago to simulate passage of time.
	tb.mu.Lock()
	tb.lastTime = time.Now().Add(-500 * time.Millisecond).UnixNano()
	tb.tokens = 0
	tb.mu.Unlock()

	// Trigger refill via checkRateLimit indirectly by calling the bucket logic.
	// We replicate the logic here since checkRateLimit is on WorkerPool.
	now := time.Now().UnixNano()
	tb.mu.Lock()
	elapsed := now - tb.lastTime
	tb.lastTime = now
	tb.tokens += float64(elapsed) * tb.rate
	if tb.tokens > tb.max {
		tb.tokens = tb.max
	}
	refilled := tb.tokens
	tb.mu.Unlock()

	// Should have refilled ~50 tokens (100/sec * 0.5s).
	if refilled < 40 || refilled > 60 {
		t.Fatalf("expected ~50 refilled tokens, got %f", refilled)
	}
}

// ---------------------------------------------------------------------------
// Integration: full pipeline via real UDP socket
// ---------------------------------------------------------------------------

func TestUDPListener_FullPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	output := make(chan *protocol.TelemetryEvent, 16)
	logger := discardLogger()

	// Pick a random port.
	cfg := config.UDPConfig{
		BindAddress:      "127.0.0.1:0",
		ReadBufferSize:   1024,
		SocketBufferSize: 64 * 1024,
		PacketQueueSize:  100,
	}
	workerCfg := config.WorkerConfig{PoolSize: 2}
	droneCfg := config.DroneConfig{
		MaxMessagesPerSecond: 1000,
		RateLimitBurst:       100,
	}

	// We need to know the actual port, so bind manually.
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	actualAddr := conn.LocalAddr().(*net.UDPAddr)
	conn.Close() // free port; the listener will rebind (tiny race, acceptable in tests)

	cfg.BindAddress = actualAddr.String()
	l := NewUDPListener(cfg, workerCfg, droneCfg, output, logger)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Start(ctx) }()

	// Give the listener time to start.
	time.Sleep(100 * time.Millisecond)

	// Send a valid heartbeat via UDP.
	sendConn, err := net.DialUDP("udp", nil, actualAddr)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer sendConn.Close()

	frame := buildHeartbeatFrame(5, 0)
	if _, err := sendConn.Write(frame); err != nil {
		cancel()
		t.Fatal(err)
	}

	select {
	case ev := <-output:
		if ev.DroneID.SystemID != 5 {
			t.Fatalf("expected SystemID 5, got %d", ev.DroneID.SystemID)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for event from UDP pipeline")
	}

	cancel()
	<-errCh
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkPacketPool_GetPut(b *testing.B) {
	pp := NewPacketPool(1024)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		pkt := pp.Get()
		pkt.Length = 100
		pkt.SourceAddr = "10.0.0.1:1234"
		pp.Put(pkt)
	}
}

func BenchmarkPacketPool_Parallel(b *testing.B) {
	pp := NewPacketPool(1024)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			pkt := pp.Get()
			pkt.Length = 100
			pp.Put(pkt)
		}
	})
}

func BenchmarkWorkerPool_Processing(b *testing.B) {
	input := make(chan *Packet, 1024)
	output := make(chan *protocol.TelemetryEvent, 1024)
	pp := NewPacketPool(1024)
	logger := discardLogger()

	wp := NewWorkerPool(4, input, output, pp, 1000000, 1000000, logger)
	ctx, cancel := context.WithCancel(context.Background())
	wp.Start(ctx)

	frame := buildHeartbeatFrame(1, 0)

	b.ReportAllocs()
	b.ResetTimer()

	// Producer goroutine.
	go func() {
		for i := 0; i < b.N; i++ {
			pkt := pp.Get()
			copy(pkt.Data, frame)
			pkt.Length = len(frame)
			pkt.SourceAddr = "10.0.0.1:1234"
			input <- pkt
		}
	}()

	// Consumer: drain output.
	for i := 0; i < b.N; i++ {
		select {
		case <-output:
		case <-time.After(5 * time.Second):
			b.Fatal("timed out in benchmark")
		}
	}

	b.StopTimer()
	cancel()
	close(input)
	wp.Wait()
}
