// Package metrics provides Prometheus instrumentation.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	UDPPacketsReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "udp_packets_received_total",
		Help: "Total UDP packets received",
	})

	UDPPacketsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "udp_packets_dropped_total",
		Help: "Total UDP packets dropped",
	}, []string{"reason"})

	MAVLinkParseErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mavlink_parse_errors_total",
		Help: "Total MAVLink parse errors",
	})

	ActiveDrones = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "active_drones",
		Help: "Number of currently connected drones",
	})

	WebSocketClientsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "websocket_clients_active",
		Help: "Number of active WebSocket clients",
	})

	EventChannelUtilization = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "event_channel_utilization",
		Help: "Fraction of telemetry event channel capacity in use",
	})

	WorkerPoolUtilization = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "worker_pool_utilization",
		Help: "Fraction of worker pool packet queue capacity in use",
	})

	RateLimitedPackets = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rate_limited_packets_total",
		Help: "Total packets dropped due to rate limiting",
	})

	TelemetryLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "telemetry_latency_seconds",
		Help:    "Latency from packet receive to state update",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 15),
	})
)
