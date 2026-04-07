package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault_ReturnsValidConfig(t *testing.T) {
	cfg := Default()

	if cfg.UDP.BindAddress == "" {
		t.Error("UDP.BindAddress is empty")
	}
	if cfg.UDP.ReadBufferSize <= 0 {
		t.Errorf("UDP.ReadBufferSize = %d, want > 0", cfg.UDP.ReadBufferSize)
	}
	if cfg.Workers.PoolSize <= 0 {
		t.Errorf("Workers.PoolSize = %d, want > 0", cfg.Workers.PoolSize)
	}
	if !cfg.MAVLink.ValidateCRC {
		t.Error("MAVLink.ValidateCRC should default to true")
	}
	if cfg.WebSocket.MaxClients <= 0 {
		t.Errorf("WebSocket.MaxClients = %d, want > 0", cfg.WebSocket.MaxClients)
	}
	if !cfg.Metrics.Enabled {
		t.Error("Metrics.Enabled should default to true")
	}
	if cfg.Debug.PprofEnabled {
		t.Error("Debug.PprofEnabled should default to false")
	}
	if cfg.Shutdown.Timeout <= 0 {
		t.Errorf("Shutdown.Timeout = %v, want > 0", cfg.Shutdown.Timeout)
	}
}

func TestDefault_SpecificValues(t *testing.T) {
	cfg := Default()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"UDP.BindAddress", cfg.UDP.BindAddress, ":14550"},
		{"UDP.ReadBufferSize", cfg.UDP.ReadBufferSize, 1024},
		{"Workers.PoolSize", cfg.Workers.PoolSize, 8},
		{"Workers.ProcessTimeout", cfg.Workers.ProcessTimeout, 10 * time.Millisecond},
		{"Drone.StaleThreshold", cfg.Drone.StaleThreshold, 30 * time.Second},
		{"Drone.MaxMessagesPerSecond", cfg.Drone.MaxMessagesPerSecond, 200},
		{"WebSocket.BindAddress", cfg.WebSocket.BindAddress, ":8080"},
		{"WebSocket.BroadcastInterval", cfg.WebSocket.BroadcastInterval, 100 * time.Millisecond},
		{"WebSocket.MaxClients", cfg.WebSocket.MaxClients, 100},
		{"PubSub.SubscriberBufferSize", cfg.PubSub.SubscriberBufferSize, 256},
		{"Metrics.BindAddress", cfg.Metrics.BindAddress, ":9090"},
		{"Shutdown.Timeout", cfg.Shutdown.Timeout, 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestLoadFile_ValidYAML(t *testing.T) {
	content := `
udp:
  bind_address: ":15000"
  read_buffer_size: 2048
workers:
  pool_size: 4
mavlink:
  validate_crc: false
websocket:
  max_clients: 50
metrics:
  enabled: false
debug:
  pprof_enabled: true
shutdown:
  timeout: 30s
`
	path := writeTempYAML(t, content)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if cfg.UDP.BindAddress != ":15000" {
		t.Errorf("UDP.BindAddress = %q, want %q", cfg.UDP.BindAddress, ":15000")
	}
	if cfg.Workers.PoolSize != 4 {
		t.Errorf("Workers.PoolSize = %d, want 4", cfg.Workers.PoolSize)
	}
	if cfg.MAVLink.ValidateCRC {
		t.Error("MAVLink.ValidateCRC = true, want false")
	}
	if cfg.WebSocket.MaxClients != 50 {
		t.Errorf("WebSocket.MaxClients = %d, want 50", cfg.WebSocket.MaxClients)
	}
	if cfg.Metrics.Enabled {
		t.Error("Metrics.Enabled = true, want false")
	}
	if !cfg.Debug.PprofEnabled {
		t.Error("Debug.PprofEnabled = false, want true")
	}
	if cfg.Shutdown.Timeout != 30*time.Second {
		t.Errorf("Shutdown.Timeout = %v, want 30s", cfg.Shutdown.Timeout)
	}
}

func TestLoadFile_InvalidYAML(t *testing.T) {
	path := writeTempYAML(t, `udp: [[[invalid`)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("LoadFile() with invalid YAML should return error")
	}
}

func TestLoadFile_NonexistentFile(t *testing.T) {
	_, err := LoadFile("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("LoadFile() with nonexistent file should return error")
	}
}

func TestLoadFile_PartialOverride(t *testing.T) {
	content := `
udp:
  bind_address: ":20000"
websocket:
  max_clients: 10
`
	path := writeTempYAML(t, content)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	defaults := Default()

	if cfg.UDP.BindAddress != ":20000" {
		t.Errorf("UDP.BindAddress = %q, want :20000", cfg.UDP.BindAddress)
	}
	if cfg.WebSocket.MaxClients != 10 {
		t.Errorf("WebSocket.MaxClients = %d, want 10", cfg.WebSocket.MaxClients)
	}
	// Non-overridden fields keep defaults
	if cfg.UDP.ReadBufferSize != defaults.UDP.ReadBufferSize {
		t.Errorf("ReadBufferSize = %d, want default %d", cfg.UDP.ReadBufferSize, defaults.UDP.ReadBufferSize)
	}
	if cfg.Workers.PoolSize != defaults.Workers.PoolSize {
		t.Errorf("Workers.PoolSize = %d, want default %d", cfg.Workers.PoolSize, defaults.Workers.PoolSize)
	}
	if cfg.Drone.StaleThreshold != defaults.Drone.StaleThreshold {
		t.Errorf("Drone.StaleThreshold = %v, want default %v", cfg.Drone.StaleThreshold, defaults.Drone.StaleThreshold)
	}
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp yaml: %v", err)
	}
	return path
}
