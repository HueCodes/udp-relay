package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsRegistered(t *testing.T) {
	// Initialize the counter vec so it appears in gather
	UDPPacketsDropped.WithLabelValues("test").Inc()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	expected := map[string]bool{
		"udp_packets_received_total":  false,
		"udp_packets_dropped_total":   false,
		"mavlink_parse_errors_total":  false,
		"active_drones":               false,
		"websocket_clients_active":    false,
		"event_channel_utilization":   false,
		"worker_pool_utilization":     false,
		"rate_limited_packets_total":  false,
		"telemetry_latency_seconds":   false,
	}

	names := make([]string, 0)
	for _, f := range families {
		names = append(names, f.GetName())
		if _, ok := expected[f.GetName()]; ok {
			expected[f.GetName()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("metric %q not found in gathered families: %v", name, names)
		}
	}
}

func TestMetricsOperations(t *testing.T) {
	// Verify metrics can be incremented without panic
	UDPPacketsReceived.Inc()
	UDPPacketsDropped.WithLabelValues("queue_full").Inc()
	MAVLinkParseErrors.Inc()
	ActiveDrones.Set(5)
	WebSocketClientsActive.Set(2)
	EventChannelUtilization.Set(0.5)
	WorkerPoolUtilization.Set(0.3)
	RateLimitedPackets.Inc()
	TelemetryLatency.Observe(0.001)
}
