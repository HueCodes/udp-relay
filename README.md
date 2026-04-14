# udp-relay

High-throughput MAVLink v2 drone telemetry aggregator written in Go.

[![CI](https://github.com/HueCodes/udp-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/HueCodes/udp-relay/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8.svg)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Architecture

```
                                   +------------------+
                                   |   Prometheus     |
                                   |   /metrics:9090  |
                                   +--------+---------+
                                            |
  UDP :14550                                |
  +--------+     +----------+     +---------+--------+     +-----------+
  | Drones |---->| Packet   |---->| Worker Pool (8)  |---->| Pub/Sub   |
  |        |     | Queue    |     |                  |     | Hub       |
  +--------+     | (10,000) |     | MAVLink Parser   |     +-----+-----+
                 +----------+     | CRC Validation   |           |
                                  | Rate Limiting    |     +-----+-----+
                                  +------------------+     | WebSocket |
                                                           | Broadcast |
                  +------------------+                     +-----+-----+
                  | Drone Registry   |<--- state updates         |
                  | (250 vehicles)   |                     +-----+-----+
                  | Ring Buffer Hist |-------------------->| Clients   |
                  +------------------+                     | REST API  |
                                                           | :8080     |
                  +------------------+                     +-----------+
                  | Health + pprof   |
                  | /api/health      |
                  +------------------+
```

## Features

**Ingestion**
- UDP listener on configurable port (default :14550) with 8MB socket buffer
- 10,000-packet queue with non-blocking drops under load
- sync.Pool packet recycling to minimize GC pressure
- Source IP/CIDR whitelisting

**Processing**
- 8-worker concurrent pool with panic recovery and restart
- MAVLink v2 frame parsing with CRC-16/MCRF4XX checksum validation
- Per-drone token bucket rate limiting (200 msg/s, 50-burst)
- Payload decoding: Heartbeat, GPS, Battery, Attitude, SysStatus
- Telemetry validation (GPS bounds, battery range)

**State Management**
- Thread-safe drone registry supporting 250 concurrent vehicles
- RWMutex for high read concurrency (broadcasts >> writes)
- Stale drone detection with configurable timeout (default 30s)
- Arm/disarm state change detection and flight mode decoding
- Per-drone historical ring buffer (configurable, default 1000 entries)

**Broadcasting**
- Fan-out pub/sub hub with configurable subscriber buffers (256 events)
- Drop-on-slow-subscriber to prevent backpressure propagation
- WebSocket server with filtered subscriptions (by drone ID, event type)
- Batched broadcasts at configurable interval (default 100ms)
- REST API for drone state, history, and GeoJSON/KML export

**Observability**
- Prometheus metrics: packets received/dropped, parse errors, active drones,
  WebSocket clients, telemetry latency histogram, rate-limited packets
- Expanded health endpoint with component status, goroutine count, heap usage
- Optional pprof endpoints on metrics port
- Structured logging (text/JSON) with configurable levels

**Deployment**
- Single static binary, ~6MB distroless Docker image
- Docker Compose with optional Prometheus + Grafana monitoring stack
- systemd service file included
- YAML config with CLI flag overrides
- Graceful shutdown with signal handling (SIGINT, SIGTERM)

## Quick Start

```bash
# Clone and build
git clone https://github.com/HueCodes/udp-relay.git
cd udp-relay
make build

# Run with built-in simulator (no real drones needed)
make simulate
```

The simulator generates realistic MAVLink traffic from configurable virtual
drones. In another terminal, connect the TUI dashboard:

```bash
make tui
```

Or query the REST API:

```bash
curl http://localhost:8080/api/drones
curl http://localhost:8080/api/health
```

## Configuration

The server loads defaults, then merges a YAML config file, then applies CLI
flags. See [config.example.yaml](config.example.yaml) for all options.

| Option | Default | Description |
|--------|---------|-------------|
| `udp.bind_address` | `:14550` | UDP listen address |
| `udp.read_buffer_size` | `1024` | Per-read buffer size (bytes) |
| `udp.socket_buffer_size` | `8388608` | OS socket buffer (8MB) |
| `udp.packet_queue_size` | `10000` | Internal packet queue depth |
| `udp.allowed_cidrs` | `[]` | Source IP whitelist (empty = all) |
| `workers.pool_size` | `8` | Concurrent worker goroutines |
| `drone.stale_threshold` | `30s` | Mark drone disconnected after |
| `drone.max_messages_per_second` | `200` | Per-drone rate limit |
| `drone.rate_limit_burst` | `50` | Token bucket burst allowance |
| `mavlink.validate_crc` | `true` | CRC-16 checksum validation |
| `websocket.bind_address` | `:8080` | HTTP/WebSocket listen address |
| `websocket.broadcast_interval` | `100ms` | Batched broadcast interval |
| `websocket.max_clients` | `100` | Max concurrent WebSocket clients |
| `pubsub.subscriber_buffer_size` | `256` | Per-subscriber event buffer |
| `pubsub.drop_on_slow_subscriber` | `true` | Drop vs. block on slow consumers |
| `metrics.enabled` | `true` | Enable Prometheus metrics |
| `metrics.bind_address` | `:9090` | Metrics server address |
| `debug.pprof_enabled` | `false` | Enable pprof endpoints |
| `shutdown.timeout` | `10s` | Graceful shutdown deadline |

CLI flags: `--config`, `--udp`, `--http`, `--workers`, `--log-level`, `--log-format`

## API Reference

### REST Endpoints

**GET /api/health** -- Server health and component status.

```json
{
  "status": "ok",
  "version": "v1.0.0",
  "timestamp": 1713000000000,
  "uptime_seconds": 3600.5,
  "components": {
    "udp": {"packets_received": 150000, "packets_dropped": 0, "parse_errors": 12},
    "drones": {"total": 5, "connected": 4, "armed": 2, "messages": 148000},
    "pubsub": {"subscribers": 1, "events_received": 148000, "events_broadcast": 148000, "events_dropped": 0},
    "websocket": {"clients": 3}
  },
  "system": {"goroutines": 24, "heap_alloc_mb": 12.4, "telemetry_chan_utilization": 0.02}
}
```

**GET /api/drones** -- All drone summaries.

```json
{
  "timestamp": 1713000000000,
  "count": 2,
  "drones": [
    {
      "system_id": 1,
      "component_id": 1,
      "connected": true,
      "armed": true,
      "flight_mode": "MISSION",
      "vehicle_type": "quadrotor",
      "lat": 37.7749,
      "lon": -122.4194,
      "alt": 50.0,
      "heading": 270.0,
      "battery_pct": 72,
      "battery_v": 16.2,
      "last_seen_ms": 1713000000000
    }
  ]
}
```

**GET /api/drones/{id}/history?last=100** -- Recent state snapshots for a drone.

**GET /api/drones/{id}/history/export?format=geojson** -- Drone trajectory as GeoJSON.

**GET /api/drones/export?format=geojson** -- All active drones as GeoJSON FeatureCollection.

**GET /api/drones/export?format=kml** -- All active drones as KML (Google Earth).

### WebSocket

Connect to `ws://localhost:8080/ws` to receive real-time state updates.

**Incoming messages** (server to client):

```json
{
  "type": "state_update",
  "timestamp": 1713000000000,
  "drones": [{"system_id": 1, "connected": true, "armed": true, "lat": 37.7749, "...": "..."}]
}
```

**Outgoing messages** (client to server) -- subscribe to specific drones:

```json
{"drone_ids": [1, 2, 3], "event_types": ["telemetry", "armed", "disarmed"]}
```

Send an empty object `{}` to reset filters and receive all drones.

## Deployment

### Docker

```bash
docker build -t udp-relay .
docker run -p 14550:14550/udp -p 8080:8080 -p 9090:9090 udp-relay
```

### Docker Compose (with monitoring)

```bash
docker compose --profile monitoring up
```

Includes Prometheus scraping on :9090 and Grafana on :3000.

### systemd

```bash
sudo cp build/drone-server /usr/local/bin/
sudo cp deploy/drone-server.service /etc/systemd/system/
sudo systemctl enable --now drone-server
```

## Tools

### Traffic Simulator (`cmd/simulator/`)

Generates realistic MAVLink v2 traffic from virtual drones with GPS waypoint
circuits, battery drain, and attitude changes. Useful for demos and load testing.

```bash
make simulate                     # 5 drones, default settings
go run ./cmd/simulator -drones 20 -rate 50  # 20 drones, 50 Hz
```

### Replay Tool (`cmd/replay/`)

Replays captured MAVLink frames at original or accelerated timing.

```bash
go run ./cmd/replay -file testdata/sample_flight.bin -speed 2.0
```

### TUI Dashboard (`cmd/tui/`)

Live terminal dashboard showing all connected drones with color-coded battery
levels, flight modes, and packet rates.

```bash
make tui
```

## Performance

Benchmarks on Apple M2 (`go test -bench=. -benchmem`):

```
MAVLink Parser
  BenchmarkParseFrameHeartbeat          24.4M ops/s    49 ns/op    427 MB/s   48 B/op   1 alloc
  BenchmarkDecodePacketHeartbeat         9.5M ops/s   127 ns/op    165 MB/s  128 B/op   3 allocs
  BenchmarkDecodePacketGlobalPosition    6.3M ops/s   192 ns/op    209 MB/s  192 B/op   3 allocs
  BenchmarkDecodePacketAttitude          6.2M ops/s   190 ns/op    210 MB/s  160 B/op   3 allocs
  BenchmarkDecodePacketBattery           5.4M ops/s   223 ns/op    216 MB/s  136 B/op   3 allocs

CRC-16/MCRF4XX
  BenchmarkCRCCalculate (50 bytes)       9.5M ops/s   125 ns/op    400 MB/s    0 B/op   0 allocs
  BenchmarkCRCThroughput (255 bytes)     1.7M ops/s   725 ns/op    352 MB/s    0 B/op   0 allocs

Drone Manager (50 drones registered)
  BenchmarkProcessEvent                  7.8M ops/s   147 ns/op     16 B/op   1 alloc
  BenchmarkGetAllSummaries               845K ops/s  1273 ns/op   4864 B/op   1 alloc
  BenchmarkDroneManagerContended         1.2M ops/s   997 ns/op   4406 B/op   1 alloc

Pub/Sub Hub (zero-alloc fan-out)
  BenchmarkBroadcast1Sub                 3.3M ops/s   368 ns/op      0 B/op   0 allocs
  BenchmarkBroadcast10Sub                972K ops/s  2085 ns/op      0 B/op   0 allocs
  BenchmarkBroadcast100Sub               60K ops/s  29.5 us/op      0 B/op   0 allocs

Packet Pool
  BenchmarkPacketPool_GetPut           154M ops/s      7.8 ns/op    0 B/op   0 allocs
  BenchmarkPacketPool_Parallel         776M ops/s      3.0 ns/op    0 B/op   0 allocs

Full Pipeline (UDP bytes -> parse -> decode -> JSON)
  BenchmarkFullPipeline                  2.9M ops/s   401 ns/op    264 B/op   5 allocs
```

## Project Structure

```
udp-relay/
  cmd/
    server/          Entry point, config loading, signal handling
    simulator/       MAVLink traffic generator for demos and testing
    replay/          Replay captured MAVLink frames
    tui/             Terminal UI dashboard (bubbletea)
  internal/
    broadcast/       WebSocket server, client management, REST API
    config/          YAML config loading with defaults
    drone/           Drone state registry, stale detection, history ring buffer
    ingest/          UDP listener, packet pool, worker pool, rate limiting
    mavlink/         MAVLink v2 parser, CRC-16, payload decoding, validation
    metrics/         Prometheus counters, gauges, histograms
    pubsub/          Fan-out event hub, subscriber management
  pkg/
    protocol/        Public MAVLink types, constants, event structures
  deploy/            systemd unit, Prometheus config
  testdata/          Sample captures for replay and testing
```

## License

[MIT](LICENSE)
