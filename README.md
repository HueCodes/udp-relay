# Drone Telemetry Aggregator

A high-performance Go server for aggregating MAVLink telemetry from PX4-based drones and broadcasting to frontend clients via WebSockets.

## Architecture

```
                              ┌─────────────────────────────────────────────────┐
                              │              DRONE FLEET                        │
                              │  ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐            │
                              │  │Drone│  │Drone│  │Drone│  │Drone│   ...      │
                              │  │ #1  │  │ #2  │  │ #3  │  │ #N  │            │
                              │  └──┬──┘  └──┬──┘  └──┬──┘  └──┬──┘            │
                              └─────┼────────┼────────┼────────┼───────────────┘
                                    │        │        │        │
                                    │   MAVLink UDP (14550)    │
                                    │        │        │        │
                              ┌─────▼────────▼────────▼────────▼───────────────┐
                              │                                                 │
                              │              UDP INGEST LAYER                   │
                              │  ┌─────────────────────────────────────────┐   │
                              │  │           Packet Queue (10k buffer)      │   │
                              │  └─────────────────┬───────────────────────┘   │
                              │                    │                            │
                              │  ┌────────┬────────┼────────┬────────┐         │
                              │  │Worker 1│Worker 2│Worker 3│Worker N│         │
                              │  │(parse) │(parse) │(parse) │(parse) │         │
                              │  └────┬───┴────┬───┴────┬───┴────┬───┘         │
                              │       │        │        │        │              │
                              │       └────────┼────────┼────────┘              │
                              │                │                                │
                              └────────────────┼────────────────────────────────┘
                                               │
                                      Telemetry Events
                                               │
                    ┌──────────────────────────┼──────────────────────────┐
                    │                          │                          │
                    ▼                          ▼                          │
          ┌─────────────────┐        ┌─────────────────┐                  │
          │  DRONE MANAGER  │        │   PUB/SUB HUB   │                  │
          │                 │        │    (Fan-Out)    │                  │
          │ ┌─────────────┐ │        │                 │                  │
          │ │  Registry   │ │        │ ┌────┐ ┌────┐   │                  │
          │ │ (RWMutex)   │ │        │ │Sub1│ │Sub2│   │                  │
          │ │             │ │        │ └──┬─┘ └──┬─┘   │                  │
          │ │ Drone #1 ───┼─┼───────▶│    │      │     │                  │
          │ │ Drone #2    │ │        │    ▼      ▼     │                  │
          │ │ Drone #N    │ │        │ ┌────┐ ┌────┐   │                  │
          │ └─────────────┘ │        │ │ WS │ │Log │   │                  │
          │                 │        │ └────┘ └────┘   │                  │
          └─────────────────┘        └─────────────────┘                  │
                    │                         │                           │
                    │                         │                           │
                    ▼                         ▼                           │
          ┌─────────────────────────────────────────────────────────┐    │
          │                   HTTP / WEBSOCKET                       │    │
          │                                                          │    │
          │   GET /api/drones     →  JSON drone list                │    │
          │   GET /api/health     →  Server health                  │    │
          │   WS  /ws            →  Real-time updates               │    │
          │                                                          │    │
          └─────────────────────────────────────────────────────────┘    │
                              │                                           │
                              ▼                                           │
                    ┌───────────────────┐                                │
                    │  FRONTEND CLIENTS │                                │
                    │  (Web Dashboard)  │                                │
                    └───────────────────┘                                │
```

## Key Design Decisions

### 1. Worker Pool Pattern for UDP Processing

The UDP listener uses a fixed-size worker pool to process incoming packets:

```go
// Non-blocking send to worker pool
select {
case packetChan <- pkt:
    // Successfully queued
default:
    // Queue full - drop packet
    l.packetsDropped.Add(1)
}
```

**Why?** This prevents a single misbehaving drone (sending malformed or high-frequency packets) from blocking the entire ingest pipeline. Each worker processes packets independently, and rate limiting is applied per-drone.

### 2. RWMutex for Drone Registry

The drone registry uses `sync.RWMutex` instead of channels for state management:

```go
type Manager struct {
    mu     sync.RWMutex
    drones map[protocol.DroneID]*State
}
```

**Why?** Telemetry reads (WebSocket broadcasts, API queries) vastly outnumber writes. RWMutex allows concurrent readers while only blocking for writes. This is more efficient than a channel-based approach for this read-heavy workload.

### 3. Fan-Out with Non-Blocking Sends

The pub/sub hub broadcasts to subscribers using non-blocking channel sends:

```go
select {
case sub.Events <- event:
    // Success
default:
    // Subscriber too slow - drop event
    sub.dropped.Add(1)
}
```

**Why?** A slow WebSocket client should never cause backpressure that affects telemetry ingestion. Dropped events are acceptable for real-time telemetry; the next update will contain current state.

### 4. Zero-Copy MAVLink Parsing

The MAVLink parser returns slices that reference the original buffer:

```go
// Zero-copy payload reference
frame.Payload = data[payloadStart:payloadEnd]
```

**Why?** At high throughput (thousands of packets/second), copying every payload would create significant GC pressure. Workers must process frames before the buffer is recycled.

### 5. Buffer Pooling

Packet buffers are pooled using `sync.Pool`:

```go
type PacketPool struct {
    pool       sync.Pool
    bufferSize int
}
```

**Why?** Reduces allocation overhead during high-throughput ingestion. Each UDP read gets a pre-allocated buffer from the pool, processes it, and returns it.

## Project Structure

```
go-drone-server/
├── cmd/
│   └── server/
│       └── main.go           # Application entry point
├── internal/
│   ├── config/
│   │   └── config.go         # Configuration management
│   ├── drone/
│   │   ├── manager.go        # Drone registry (thread-safe)
│   │   └── state.go          # Drone state definitions
│   ├── ingest/
│   │   ├── packet.go         # Packet buffer pooling
│   │   └── udp.go            # UDP listener + worker pool
│   ├── mavlink/
│   │   └── parser.go         # MAVLink v2 frame parser
│   ├── pubsub/
│   │   └── hub.go            # Fan-out event distribution
│   └── broadcast/
│       └── websocket.go      # WebSocket server
├── pkg/
│   └── protocol/
│       └── mavlink.go        # Public MAVLink types
├── Makefile
├── go.mod
└── README.md
```

## Quick Start

### Build

```bash
make build
```

### Run

```bash
# Default settings (UDP :14550, HTTP :8080)
make run

# Custom ports
./build/drone-server -udp=:14550 -http=:8080

# Debug logging
./build/drone-server -log-level=debug
```

### Test with PX4 SITL

1. Start the telemetry server:
   ```bash
   make run
   ```

2. Start PX4 SITL (in your PX4-Autopilot directory):
   ```bash
   make px4_sitl gazebo-classic
   ```

3. The server will automatically receive telemetry on port 14550.

4. Check the API:
   ```bash
   curl http://localhost:8080/api/drones
   curl http://localhost:8080/api/health
   ```

## API Reference

### GET /api/drones

Returns current state of all connected drones.

```json
{
  "timestamp": 1702847123456,
  "count": 2,
  "drones": [
    {
      "system_id": 1,
      "component_id": 1,
      "connected": true,
      "armed": false,
      "flight_mode": "POSITION",
      "vehicle_type": "quadrotor",
      "lat": 47.397742,
      "lon": 8.545594,
      "alt": 488.5,
      "heading": 45.0,
      "battery_pct": 85,
      "battery_v": 22.4,
      "last_seen_ms": 1702847123400
    }
  ]
}
```

### GET /api/health

Returns server health status.

```json
{
  "status": "ok",
  "timestamp": 1702847123456,
  "connected_drones": 2,
  "total_drones": 3,
  "total_messages": 15420
}
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `-udp` | `:14550` | UDP listen address for MAVLink |
| `-http` | `:8080` | HTTP listen address for WebSocket/API |
| `-workers` | `8` | Number of packet processing workers |
| `-log-level` | `info` | Log level (debug, info, warn, error) |
| `-log-format` | `text` | Log format (text, json) |

## Performance Considerations

- **UDP Buffer**: 8MB socket buffer handles burst traffic
- **Packet Queue**: 10,000 packet buffer absorbs spikes
- **Rate Limiting**: 200 msg/sec per drone prevents flooding
- **Worker Pool**: 8 workers for parallel packet processing
- **Broadcast Interval**: 100ms batching reduces WebSocket overhead

## Future Enhancements

- [ ] Full WebSocket implementation (gorilla/websocket)
- [ ] Message persistence (time-series database)
- [ ] Prometheus metrics endpoint
- [ ] MAVLink command sending (bidirectional)
- [ ] Geographic fencing and alerts
- [ ] Multi-datacenter replication

## License

MIT
