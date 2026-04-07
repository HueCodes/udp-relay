# Drone Telemetry Aggregator

A high-performance Go server for aggregating MAVLink telemetry from PX4-based drones and broadcasting to frontend clients via WebSockets.

## Architecture

```
DRONE FLEET (MAVLink UDP :14550)
         |
   UDP INGEST LAYER
   - Packet Queue (10k buffer)
   - Worker Pool (N goroutines, token bucket rate limiting)
   - MAVLink v2 parser with CRC-16/MCRF4XX validation
   - Source IP/CIDR whitelist
         |
   Telemetry Events (validated)
         |
    +----------+----------+
    |                     |
DRONE MANAGER         PUB/SUB HUB
(RWMutex registry)    (fan-out, non-blocking)
- State tracking       - Subscriber management
- Arm/disarm detect    - Slow subscriber drops
- Stale detection           |
         |              WebSocket Broadcaster
    HTTP/WS SERVER        |
    - GET /api/drones     WebSocket Clients
    - GET /api/health     (filtered by drone ID)
    - WS  /ws
    - GET /metrics (Prometheus, :9090)
```

## Quick Start

```bash
# Build and run with defaults (UDP :14550, HTTP :8080, Metrics :9090)
make build && ./build/drone-server

# With config file
./build/drone-server -config config.example.yaml

# With CLI overrides
./build/drone-server -udp=:14550 -http=:8080 -log-level=debug -log-format=json

# Docker
docker compose up -d

# Docker with monitoring stack (Prometheus + Grafana)
docker compose --profile monitoring up -d
```

## Configuration

All settings can be configured via YAML file (`-config` flag) with CLI flag overrides. See `config.example.yaml` for all options with defaults.

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | | Path to YAML config file |
| `-udp` | `:14550` | UDP listen address |
| `-http` | `:8080` | HTTP/WebSocket listen address |
| `-workers` | `8` | Packet processing workers |
| `-log-level` | `info` | Log level (debug, info, warn, error) |
| `-log-format` | `text` | Log format (text, json) |

### Config Reference

```yaml
udp:
  bind_address: ":14550"
  read_buffer_size: 1024        # Per-packet read buffer
  socket_buffer_size: 8388608   # OS socket buffer (8MB)
  packet_queue_size: 10000      # Max queued packets
  allowed_cidrs: []             # Source IP whitelist (empty = accept all)

workers:
  pool_size: 8
  process_timeout: 10ms

drone:
  stale_check_interval: 10s     # How often to check for stale drones
  stale_threshold: 30s          # Time before drone marked disconnected
  max_messages_per_second: 200  # Per-drone rate limit
  rate_limit_burst: 50          # Burst allowance above sustained rate

mavlink:
  validate_crc: true            # CRC-16/MCRF4XX validation

websocket:
  bind_address: ":8080"
  broadcast_interval: 100ms     # State update frequency (10 Hz)
  write_timeout: 10s
  max_message_size: 4096
  max_clients: 100

pubsub:
  subscriber_buffer_size: 256
  drop_on_slow_subscriber: true

metrics:
  enabled: true
  bind_address: ":9090"

debug:
  pprof_enabled: false          # /debug/pprof endpoints on metrics server

shutdown:
  timeout: 10s
```

## REST API

### GET /api/drones

Returns current state of all known drones.

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

Returns per-component health, system stats, uptime, and version.

```json
{
  "status": "ok",
  "version": "v1.0.0",
  "timestamp": 1702847123456,
  "uptime_seconds": 3600.5,
  "components": {
    "udp": {
      "packets_received": 150000,
      "packets_dropped": 12,
      "parse_errors": 3
    },
    "drones": {
      "total": 5,
      "connected": 3,
      "armed": 1,
      "messages": 150000
    },
    "pubsub": {
      "subscribers": 2,
      "events_received": 149985,
      "events_broadcast": 299970,
      "events_dropped": 0
    },
    "websocket": {
      "clients": 2
    }
  },
  "system": {
    "goroutines": 24,
    "heap_alloc_mb": 12.5,
    "telemetry_chan_utilization": 0.02
  }
}
```

### GET /metrics

Prometheus metrics endpoint (on metrics server, default `:9090`).

Available metrics:
- `udp_packets_received_total` - Total UDP packets received
- `udp_packets_dropped_total{reason}` - Dropped packets by reason
- `mavlink_parse_errors_total` - MAVLink parse errors
- `active_drones` - Currently connected drones
- `websocket_clients_active` - Active WebSocket connections
- `event_channel_utilization` - Telemetry channel usage
- `worker_pool_utilization` - Packet queue usage
- `rate_limited_packets_total` - Rate-limited packets
- `telemetry_latency_seconds` - Processing latency histogram

## WebSocket Protocol

### Connect

```
ws://localhost:8080/ws
```

### Subscribe (Optional Filter)

Send a JSON message to filter updates to specific drones:

```json
{
  "drone_ids": [1, 3, 5],
  "event_types": ["telemetry", "armed", "disarmed"]
}
```

Send an empty object or omit to receive all updates.

### Receive State Updates

The server sends JSON state updates at the configured broadcast interval:

```json
{
  "type": "state_update",
  "timestamp": 1702847123456,
  "drones": [
    {
      "system_id": 1,
      "connected": true,
      "armed": true,
      "flight_mode": "MISSION",
      "lat": 47.397742,
      "lon": 8.545594,
      "alt": 488.5,
      "heading": 90.0,
      "battery_pct": 72,
      "last_seen_ms": 1702847123400
    }
  ]
}
```

### Keepalive

The server sends WebSocket pings every 30 seconds. Clients that fail to respond are disconnected.

## Deployment

### Docker

```bash
docker build -t drone-server .
docker run -p 14550:14550/udp -p 8080:8080 -p 9090:9090 drone-server
```

### Docker Compose

```bash
# Server only
docker compose up -d

# With Prometheus + Grafana
docker compose --profile monitoring up -d
# Grafana: http://localhost:3000 (admin/admin)
# Prometheus: http://localhost:9091
```

### systemd

```bash
sudo cp deploy/drone-server.service /etc/systemd/system/
sudo useradd -r -s /bin/false drone-server
sudo cp build/drone-server /usr/local/bin/
sudo mkdir -p /etc/drone-server
sudo cp config.example.yaml /etc/drone-server/config.yaml
sudo systemctl enable --now drone-server
```

## Monitoring

### Useful Prometheus Queries

```promql
# Packet drop rate
rate(udp_packets_dropped_total[5m])

# Parse error rate
rate(mavlink_parse_errors_total[5m])

# P99 telemetry latency
histogram_quantile(0.99, rate(telemetry_latency_seconds_bucket[5m]))

# Active drone count
active_drones

# WebSocket client count
websocket_clients_active
```

## Troubleshooting

**No packets received**: Check firewall allows UDP 14550. Verify drone is sending to the correct address. Try `tcpdump -i any udp port 14550`.

**High packet drops**: Increase `udp.socket_buffer_size` and `udp.packet_queue_size`. Add more workers.

**Drones going stale**: Increase `drone.stale_threshold`. Check network reliability.

**WebSocket not connecting**: Verify max_clients limit not reached. Check `/api/health` for server status.

**High memory usage**: Check goroutine count in `/api/health`. Enable pprof (`debug.pprof_enabled: true`) and profile with `go tool pprof`.

## License

MIT
