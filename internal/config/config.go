// Package config provides configuration management for the drone server.
package config

import (
	"time"
)

// Config holds all server configuration parameters.
type Config struct {
	// UDP ingest settings
	UDP UDPConfig

	// Worker pool settings
	Workers WorkerConfig

	// Drone management settings
	Drone DroneConfig

	// MAVLink parsing settings
	MAVLink MAVLinkConfig

	// WebSocket broadcast settings
	WebSocket WebSocketConfig

	// Pub/Sub hub settings
	PubSub PubSubConfig
}

// UDPConfig contains UDP listener settings.
type UDPConfig struct {
	// Address to bind the UDP listener (e.g., ":14550")
	BindAddress string

	// Size of the UDP read buffer per packet
	ReadBufferSize int

	// OS-level socket receive buffer size
	SocketBufferSize int

	// Maximum packets to queue before applying backpressure
	PacketQueueSize int

	// Allowed source CIDRs (empty = accept all)
	AllowedCIDRs []string
}

// WorkerConfig contains worker pool settings.
type WorkerConfig struct {
	// Number of packet processing workers
	PoolSize int

	// Maximum time to process a single packet before logging a warning
	ProcessTimeout time.Duration
}

// DroneConfig contains drone registry settings.
type DroneConfig struct {
	// How often to check for stale drones
	StaleCheckInterval time.Duration

	// Time after last message before a drone is considered stale
	StaleThreshold time.Duration

	// Maximum messages per second from a single drone before rate limiting
	MaxMessagesPerSecond int

	// Burst allowance above the sustained rate
	RateLimitBurst int

	// Rate limit window duration
	RateLimitWindow time.Duration
}

// MAVLinkConfig contains MAVLink parsing settings.
type MAVLinkConfig struct {
	// Whether to validate CRC-16/MCRF4XX checksums
	ValidateCRC bool
}

// WebSocketConfig contains WebSocket server settings.
type WebSocketConfig struct {
	// Address to bind the WebSocket server
	BindAddress string

	// Broadcast interval for state updates
	BroadcastInterval time.Duration

	// Write timeout for client connections
	WriteTimeout time.Duration

	// Maximum message size from clients
	MaxMessageSize int64

	// Maximum concurrent WebSocket clients (0 = unlimited)
	MaxClients int
}

// PubSubConfig contains pub/sub hub settings.
type PubSubConfig struct {
	// Buffer size for subscriber channels
	SubscriberBufferSize int

	// Whether to drop events on slow subscribers (vs blocking)
	DropOnSlowSubscriber bool
}

// Default returns a production-ready default configuration.
func Default() Config {
	return Config{
		UDP: UDPConfig{
			BindAddress:      ":14550",
			ReadBufferSize:   1024,                // MAVLink max frame ~280 bytes
			SocketBufferSize: 8 * 1024 * 1024,     // 8MB socket buffer
			PacketQueueSize:  10000,               // Queue up to 10k packets
		},
		Workers: WorkerConfig{
			PoolSize:       8,                     // 8 worker goroutines
			ProcessTimeout: 10 * time.Millisecond, // Warn if processing takes >10ms
		},
		Drone: DroneConfig{
			StaleCheckInterval:   10 * time.Second,
			StaleThreshold:       30 * time.Second,
			MaxMessagesPerSecond: 200,
			RateLimitBurst:       50,
			RateLimitWindow:      time.Second,
		},
		MAVLink: MAVLinkConfig{
			ValidateCRC: true,
		},
		WebSocket: WebSocketConfig{
			BindAddress:       ":8080",
			BroadcastInterval: 100 * time.Millisecond, // 10 Hz updates
			WriteTimeout:      10 * time.Second,
			MaxMessageSize:    4096,
			MaxClients:        100,
		},
		PubSub: PubSubConfig{
			SubscriberBufferSize: 256,
			DropOnSlowSubscriber: true, // Non-blocking fan-out
		},
	}
}
