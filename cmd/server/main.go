// Package main provides the entry point for the drone telemetry server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/hugh/go-drone-server/internal/broadcast"
	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/internal/ingest"
	"github.com/hugh/go-drone-server/internal/pubsub"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

func main() {
	// Parse command-line flags
	var (
		udpAddr   = flag.String("udp", ":14550", "UDP listen address for MAVLink")
		httpAddr  = flag.String("http", ":8080", "HTTP listen address for WebSocket/API")
		workers   = flag.Int("workers", 8, "Number of packet processing workers")
		logLevel  = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
		logFormat = flag.String("log-format", "text", "Log format (text, json)")
	)
	flag.Parse()

	// Configure structured logging
	logger := configureLogger(*logLevel, *logFormat)

	logger.Info("Starting Drone Telemetry Server",
		"udp_address", *udpAddr,
		"http_address", *httpAddr,
		"workers", *workers,
	)

	// Load configuration with command-line overrides
	cfg := config.Default()
	cfg.UDP.BindAddress = *udpAddr
	cfg.WebSocket.BindAddress = *httpAddr
	cfg.Workers.PoolSize = *workers

	// Create root context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Initialize the processing pipeline
	//
	// Architecture:
	//
	//   UDP Socket
	//       │
	//       ▼
	//   ┌─────────────────┐
	//   │  Packet Queue   │  (buffered channel)
	//   └────────┬────────┘
	//            │
	//   ┌────────▼────────┐
	//   │   Worker Pool   │  (N goroutines parsing MAVLink)
	//   └────────┬────────┘
	//            │
	//   ┌────────▼────────┐
	//   │ Telemetry Chan  │  (parsed events)
	//   └────────┬────────┘
	//            │
	//      ┌─────┴─────┐
	//      │           │
	//      ▼           ▼
	//  ┌───────┐  ┌─────────┐
	//  │ Drone │  │ Pub/Sub │
	//  │Manager│  │   Hub   │
	//  └───────┘  └────┬────┘
	//                  │
	//            ┌─────┴─────┐
	//            │           │
	//            ▼           ▼
	//      ┌──────────┐  ┌────────┐
	//      │ WebSocket│  │ Logger │
	//      │Broadcaster│  │(future)│
	//      └──────────┘  └────────┘

	// Channel for parsed telemetry events (from workers to consumers)
	telemetryChan := make(chan *protocol.TelemetryEvent, 1000)

	// Channel for drone state updates (from manager to pub/sub)
	updateChan := make(chan drone.StateUpdate, 256)

	// Create core components
	droneManager := drone.NewManager(cfg.Drone, updateChan, logger)
	hub := pubsub.NewHub(cfg.PubSub, telemetryChan, logger)
	udpListener := ingest.NewUDPListener(cfg.UDP, cfg.Workers, telemetryChan, logger)
	wsServer := broadcast.NewWebSocketServer(cfg.WebSocket, droneManager, hub, logger)

	// Start components in dependency order
	droneManager.Start(ctx)
	hub.Start(ctx)

	// Start event processor (connects telemetry to drone manager)
	go eventProcessor(ctx, telemetryChan, droneManager, logger)

	// Start WebSocket server (non-blocking)
	go func() {
		if err := wsServer.Start(ctx); err != nil {
			logger.Error("WebSocket server error", "error", err)
		}
	}()

	// Start UDP listener (blocks until context cancelled)
	go func() {
		if err := udpListener.Start(ctx); err != nil {
			logger.Error("UDP listener error", "error", err)
			cancel() // Trigger shutdown on fatal error
		}
	}()

	// Print startup banner
	printBanner(logger, *udpAddr, *httpAddr)

	// Wait for shutdown signal
	sig := <-sigChan
	logger.Info("Received shutdown signal", "signal", sig)

	// Initiate graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Cancel main context to stop all components
	cancel()

	// Give components time to drain
	<-shutdownCtx.Done()

	// Stop components in reverse order
	wsServer.Stop()
	hub.Stop()
	droneManager.Stop()

	// Print final statistics
	printStats(logger, droneManager, hub, udpListener)

	logger.Info("Server shutdown complete")
}

// eventProcessor routes telemetry events to the drone manager.
// This runs in its own goroutine to decouple the ingest pipeline from state management.
func eventProcessor(
	ctx context.Context,
	events <-chan *protocol.TelemetryEvent,
	manager *drone.Manager,
	logger *slog.Logger,
) {
	logger = logger.With("component", "event_processor")
	logger.Debug("event processor started")

	for {
		if !runEventProcessor(ctx, events, manager, logger) {
			logger.Debug("event processor stopped")
			return
		}
	}
}

func runEventProcessor(
	ctx context.Context,
	events <-chan *protocol.TelemetryEvent,
	manager *drone.Manager,
	logger *slog.Logger,
) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("event processor panicked, restarting",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			panicked = true
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return false
		case event, ok := <-events:
			if !ok {
				return false
			}
			manager.ProcessEvent(event)
		}
	}
}

// configureLogger creates a structured logger based on settings.
func configureLogger(level, format string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: logLevel,
	}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// printBanner prints the startup banner.
func printBanner(logger *slog.Logger, udpAddr, httpAddr string) {
	banner := `
╔═══════════════════════════════════════════════════════════╗
║         DRONE TELEMETRY AGGREGATOR                        ║
║         High-Performance MAVLink Server                   ║
╠═══════════════════════════════════════════════════════════╣
║  UDP Ingest:  %-43s ║
║  HTTP/WS:     %-43s ║
╚═══════════════════════════════════════════════════════════╝
`
	// Log individual lines for structured logging compatibility
	logger.Info("=== DRONE TELEMETRY AGGREGATOR ===")
	logger.Info("Server ready",
		"udp_ingest", udpAddr,
		"http_api", httpAddr,
	)
	logger.Info("Press Ctrl+C to shutdown")

	// Also print banner to stdout for interactive use
	os.Stdout.WriteString("\n")
	os.Stdout.WriteString("╔═══════════════════════════════════════════════════════════╗\n")
	os.Stdout.WriteString("║         DRONE TELEMETRY AGGREGATOR                        ║\n")
	os.Stdout.WriteString("║         High-Performance MAVLink Server                   ║\n")
	os.Stdout.WriteString("╠═══════════════════════════════════════════════════════════╣\n")
	os.Stdout.WriteString("║  UDP:  " + udpAddr + "                                           ║\n")
	os.Stdout.WriteString("║  HTTP: " + httpAddr + "                                           ║\n")
	os.Stdout.WriteString("╚═══════════════════════════════════════════════════════════╝\n\n")

	_ = banner // Suppress unused warning
}

// printStats prints final statistics on shutdown.
func printStats(
	logger *slog.Logger,
	manager *drone.Manager,
	hub *pubsub.Hub,
	listener *ingest.UDPListener,
) {
	managerStats := manager.Stats()
	hubStats := hub.Stats()
	listenerStats := listener.Stats()

	logger.Info("Final Statistics",
		"total_drones", managerStats.TotalDrones,
		"connected_drones", managerStats.ConnectedDrones,
		"armed_drones", managerStats.ArmedDrones,
		"total_messages", managerStats.TotalMessages,
		"packets_received", listenerStats.PacketsReceived,
		"packets_dropped", listenerStats.PacketsDropped,
		"parse_errors", listenerStats.ParseErrors,
		"events_broadcast", hubStats.EventsBroadcast,
		"events_dropped", hubStats.EventsDropped,
	)
}
