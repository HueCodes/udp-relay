# Productionize UDP-Relay (Drone Telemetry Aggregator)

Work through this plan to completion. Only ask for approval if you encounter a genuinely ambiguous design decision or something that could break the project's core architecture. Otherwise, use your best judgment and keep moving.

## Context

This is a Go server (`~/Dev/projects/udp-relay`) that ingests MAVLink v2 telemetry from PX4 drones over UDP, maintains per-drone state, and broadcasts to frontend clients. The architecture is solid (worker pool, RWMutex registry, non-blocking pub/sub, buffer pooling) but it's missing critical production hardening. Zero external dependencies currently (pure stdlib). Go 1.25.5.

## Phase 1: Critical Bug Fixes

### 1.1 Panic Recovery in All Goroutines
- Add `defer recover()` with structured error logging + stack traces in:
  - Worker goroutines (`internal/ingest/udp.go` worker function)
  - Event processor goroutine (`cmd/server/main.go`)
  - Broadcast loop (`internal/broadcast/websocket.go`)
  - Pub/sub hub broadcast goroutine (`internal/pubsub/hub.go`)
  - Stale checker goroutine (`internal/drone/manager.go`)
- On panic recovery, log the error and restart the goroutine (don't just silently die)

### 1.2 Fix Pub/Sub Double-Close Race
- `pubsub/hub.go`: `Stop()` closes all subscriber channels AND `Unsubscribe()` also closes them. This is a double-close panic waiting to happen.
- Fix: Use `sync.Once` per subscriber channel, or track closed state with an atomic bool. Ensure `Stop()` and `Unsubscribe()` are safe to call concurrently and in any order.

### 1.3 Silent Drop Logging
- `drone/manager.go` ~line 162: When the update channel is full and updates are dropped, log at WARN level (not every time -- use a counter and log every Nth drop or use rate-limited logging)
- Same for event channel drops in the worker pool

## Phase 2: Input Validation & Security

### 2.1 Telemetry Data Validation
- Validate GPS coordinates: lat [-90, 90], lon [-180, 180]
- Clamp battery percentage to [0, 100]
- Validate heading [0, 360)
- Validate altitude within reasonable bounds (reject clearly corrupted values like > 100km)
- Validate system IDs are in MAVLink valid range (1-250)
- Log and drop invalid values rather than storing them

### 2.2 Source IP Filtering
- Add optional source IP/CIDR whitelist to config
- When configured, drop packets from non-whitelisted sources
- When not configured, accept all (current behavior, for backwards compat)

### 2.3 CRC-16 Validation
- Implement MAVLink CRC-16/MCRF4XX checksum validation in `mavlink/parser.go` (there's a TODO at line ~127)
- Make it configurable: enabled by default, can be disabled for performance-critical deployments
- Include the CRC extra byte per message type (MAVLink spec requirement)

### 2.4 Rate Limiting Improvements
- Move the hardcoded 200 msg/sec rate limit (`udp.go` line ~312) to config
- Add burst allowance (token bucket instead of simple counter)
- Make it configurable per-drone or globally

## Phase 3: WebSocket Implementation

The `/ws` endpoint currently returns 501. Implement it properly:

- Use `nhooyr.io/websocket` (modern, maintained, context-aware) or `gorilla/websocket`
- Subscribe each WebSocket client to the pub/sub hub
- Send JSON-encoded drone state updates at the configured broadcast interval (100ms)
- Handle client disconnection gracefully (unsubscribe from hub, close cleanly)
- Add ping/pong keepalive
- Add configurable max clients limit
- Support message filtering (client can subscribe to specific drone IDs or event types via initial JSON message)

## Phase 4: Configuration & Observability

### 4.1 Config File Support
- Add YAML config file support alongside existing CLI flags
- CLI flags override config file values
- Add a `-config` flag to specify config file path
- Make ALL currently-hardcoded values configurable (socket buffer size, queue sizes, stale threshold, broadcast interval, rate limits, etc.)

### 4.2 Prometheus Metrics
- Add `/metrics` endpoint with standard Prometheus exposition format
- Key metrics:
  - `udp_packets_received_total` (counter)
  - `udp_packets_dropped_total` (counter, with reason label)
  - `mavlink_parse_errors_total` (counter)
  - `active_drones` (gauge)
  - `websocket_clients_active` (gauge)
  - `event_channel_utilization` (gauge, percentage full)
  - `worker_pool_utilization` (gauge)
  - `rate_limited_packets_total` (counter, per drone)
  - `telemetry_latency_seconds` (histogram, ingest-to-broadcast)
- Use the `prometheus/client_golang` library

### 4.3 Structured Health Endpoint
- Expand `/api/health` to include:
  - Per-component health (UDP listener, worker pool, pub/sub hub, WebSocket server)
  - Uptime
  - Memory usage (from runtime)
  - Goroutine count
  - Channel buffer utilization percentages
  - Version info

### 4.4 pprof Endpoint
- Add optional `/debug/pprof` endpoints (disabled by default, enabled via config flag)
- CPU, memory, goroutine, block profiling

## Phase 5: Comprehensive Test Suite

### 5.1 Unit Tests
Write thorough unit tests for every package:

- **mavlink/parser_test.go**: Valid frame parsing for all 5 message types, invalid magic byte, truncated frames, CRC validation, fuzz testing with random bytes
- **ingest/udp_test.go**: Worker pool lifecycle, packet queue overflow behavior, rate limiter window boundaries, graceful shutdown
- **drone/manager_test.go**: State transitions (connect/disconnect/reconnect), stale detection timing, concurrent read/write safety (run with `-race`), flight mode decoding for all PX4 modes
- **drone/state_test.go**: Clone correctness, update application
- **pubsub/hub_test.go**: Subscribe/unsubscribe, broadcast delivery, slow subscriber drop behavior, concurrent Stop()/Unsubscribe() safety
- **broadcast/websocket_test.go**: HTTP endpoint responses, WebSocket connection lifecycle, client filtering, max client enforcement
- **config/config_test.go**: Default values, YAML parsing, flag override precedence

### 5.2 Integration Tests
- Create a mock UDP drone sender that emits realistic MAVLink packets
- Test full pipeline: UDP ingest -> parse -> state update -> WebSocket broadcast
- Test with multiple concurrent simulated drones (10+)
- Test graceful shutdown under load (no goroutine leaks, no panics)
- Test reconnection scenarios

### 5.3 Benchmarks
- MAVLink parser throughput (frames/sec)
- Full pipeline throughput (UDP packet to state update)
- Drone manager lookup latency under contention
- Pub/sub broadcast latency with N subscribers
- JSON serialization of drone summaries

### 5.4 Race Detection
- Ensure ALL tests pass with `-race` flag
- Specifically test the pub/sub shutdown race that was identified

## Phase 6: Deployment & Operations

### 6.1 Dockerfile
- Multi-stage build (build stage + scratch/distroless runtime)
- Non-root user
- Health check instruction
- Expose UDP 14550 and HTTP 8080

### 6.2 Docker Compose
- Service definition with resource limits
- Optional Prometheus + Grafana stack for monitoring
- Volume mounts for config

### 6.3 Systemd Service File
- `drone-telemetry.service` unit file
- Restart on failure
- Resource limits (memory, file descriptors)
- Proper shutdown signal handling (SIGTERM)

### 6.4 Graceful Shutdown Improvements
- Ensure shutdown order: stop accepting new UDP packets -> drain worker pool -> flush pending WebSocket broadcasts -> close WebSocket connections -> stop HTTP server
- Add configurable shutdown timeout (currently hardcoded 10s)
- Log shutdown progress

## Phase 7: Documentation

### 7.1 API Documentation
- Document all REST endpoints with request/response examples
- Document WebSocket protocol (connection, message format, filtering)
- Include error response formats

### 7.2 Update README
- Add configuration reference (all options with defaults)
- Add deployment section (Docker, systemd)
- Add monitoring section (Prometheus queries, Grafana dashboard)
- Add troubleshooting section (common issues)

## Phase 8: CI/CD

### 8.1 GitHub Actions Workflow
- On push/PR: lint (golangci-lint), vet, test with race detector, build
- Coverage reporting (fail if below 70%)
- Benchmark comparison on PRs
- Docker image build and push on tag

### 8.2 Linting
- Add `.golangci.yml` with strict settings
- Fix any linting issues found

## General Guidelines

- Keep the zero-dependency philosophy where possible, but add dependencies when they provide clear value (WebSocket library, Prometheus client, YAML parser)
- Maintain the existing architecture patterns (worker pool, non-blocking sends, buffer pooling)
- All new code must be race-condition safe
- Use structured logging (slog) consistently
- Write idiomatic Go -- no over-engineering, no unnecessary abstractions
- Run `go vet` and `golangci-lint` after each phase
- Run tests with `-race` after each phase
- Commit after each phase with a clear message describing what was done
