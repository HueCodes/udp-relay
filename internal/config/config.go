// Package config provides configuration management for the drone server.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all server configuration parameters.
type Config struct {
	UDP       UDPConfig       `yaml:"udp"`
	Workers   WorkerConfig    `yaml:"workers"`
	Drone     DroneConfig     `yaml:"drone"`
	MAVLink   MAVLinkConfig   `yaml:"mavlink"`
	WebSocket WebSocketConfig `yaml:"websocket"`
	PubSub    PubSubConfig    `yaml:"pubsub"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Debug     DebugConfig     `yaml:"debug"`
	Shutdown  ShutdownConfig  `yaml:"shutdown"`
}

// UDPConfig contains UDP listener settings.
type UDPConfig struct {
	BindAddress      string   `yaml:"bind_address"`
	ReadBufferSize   int      `yaml:"read_buffer_size"`
	SocketBufferSize int      `yaml:"socket_buffer_size"`
	PacketQueueSize  int      `yaml:"packet_queue_size"`
	AllowedCIDRs     []string `yaml:"allowed_cidrs"`
}

// WorkerConfig contains worker pool settings.
type WorkerConfig struct {
	PoolSize       int           `yaml:"pool_size"`
	ProcessTimeout time.Duration `yaml:"process_timeout"`
}

// DroneConfig contains drone registry settings.
type DroneConfig struct {
	StaleCheckInterval   time.Duration `yaml:"stale_check_interval"`
	StaleThreshold       time.Duration `yaml:"stale_threshold"`
	MaxMessagesPerSecond int           `yaml:"max_messages_per_second"`
	RateLimitBurst       int           `yaml:"rate_limit_burst"`
	RateLimitWindow      time.Duration `yaml:"rate_limit_window"`
	HistorySize          int           `yaml:"history_size"`
}

// MAVLinkConfig contains MAVLink parsing settings.
type MAVLinkConfig struct {
	ValidateCRC bool `yaml:"validate_crc"`
}

// WebSocketConfig contains WebSocket server settings.
type WebSocketConfig struct {
	BindAddress       string        `yaml:"bind_address"`
	BroadcastInterval time.Duration `yaml:"broadcast_interval"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	MaxMessageSize    int64         `yaml:"max_message_size"`
	MaxClients        int           `yaml:"max_clients"`
}

// PubSubConfig contains pub/sub hub settings.
type PubSubConfig struct {
	SubscriberBufferSize int  `yaml:"subscriber_buffer_size"`
	DropOnSlowSubscriber bool `yaml:"drop_on_slow_subscriber"`
}

// MetricsConfig contains Prometheus metrics settings.
type MetricsConfig struct {
	Enabled     bool   `yaml:"enabled"`
	BindAddress string `yaml:"bind_address"`
}

// DebugConfig contains debug/profiling settings.
type DebugConfig struct {
	PprofEnabled bool `yaml:"pprof_enabled"`
}

// ShutdownConfig contains graceful shutdown settings.
type ShutdownConfig struct {
	Timeout time.Duration `yaml:"timeout"`
}

// Default returns a production-ready default configuration.
func Default() Config {
	return Config{
		UDP: UDPConfig{
			BindAddress:      ":14550",
			ReadBufferSize:   1024,
			SocketBufferSize: 8 * 1024 * 1024,
			PacketQueueSize:  10000,
		},
		Workers: WorkerConfig{
			PoolSize:       8,
			ProcessTimeout: 10 * time.Millisecond,
		},
		Drone: DroneConfig{
			StaleCheckInterval:   10 * time.Second,
			StaleThreshold:       30 * time.Second,
			MaxMessagesPerSecond: 200,
			RateLimitBurst:       50,
			RateLimitWindow:      time.Second,
			HistorySize:          1000,
		},
		MAVLink: MAVLinkConfig{
			ValidateCRC: true,
		},
		WebSocket: WebSocketConfig{
			BindAddress:       ":8080",
			BroadcastInterval: 100 * time.Millisecond,
			WriteTimeout:      10 * time.Second,
			MaxMessageSize:    4096,
			MaxClients:        100,
		},
		PubSub: PubSubConfig{
			SubscriberBufferSize: 256,
			DropOnSlowSubscriber: true,
		},
		Metrics: MetricsConfig{
			Enabled:     true,
			BindAddress: ":9090",
		},
		Debug: DebugConfig{
			PprofEnabled: false,
		},
		Shutdown: ShutdownConfig{
			Timeout: 10 * time.Second,
		},
	}
}

// LoadFile reads a YAML config file and merges it over defaults.
func LoadFile(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config file: %w", err)
	}

	return cfg, nil
}
