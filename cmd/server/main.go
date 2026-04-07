// Package main provides the entry point for the drone telemetry server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hugh/go-drone-server/internal/broadcast"
	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/internal/ingest"
	_ "github.com/hugh/go-drone-server/internal/metrics" // register metrics
	"github.com/hugh/go-drone-server/internal/pubsub"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// Set at build time via -ldflags.
var version = "dev"

func main() {
	var (
		configFile = flag.String("config", "", "Path to YAML config file")
		udpAddr    = flag.String("udp", "", "UDP listen address (overrides config)")
		httpAddr   = flag.String("http", "", "HTTP listen address (overrides config)")
		workers    = flag.Int("workers", 0, "Number of workers (overrides config)")
		logLevel   = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
		logFormat  = flag.String("log-format", "text", "Log format (text, json)")
	)
	flag.Parse()

	logger := configureLogger(*logLevel, *logFormat)

	// Load config: file defaults -> YAML file -> CLI overrides
	var cfg config.Config
	var err error
	if *configFile != "" {
		cfg, err = config.LoadFile(*configFile)
		if err != nil {
			logger.Error("failed to load config file", "path", *configFile, "error", err)
			os.Exit(1)
		}
		logger.Info("loaded config file", "path", *configFile)
	} else {
		cfg = config.Default()
	}

	// CLI flag overrides
	if *udpAddr != "" {
		cfg.UDP.BindAddress = *udpAddr
	}
	if *httpAddr != "" {
		cfg.WebSocket.BindAddress = *httpAddr
	}
	if *workers > 0 {
		cfg.Workers.PoolSize = *workers
	}

	logger.Info("starting drone telemetry server",
		"version", version,
		"udp_address", cfg.UDP.BindAddress,
		"http_address", cfg.WebSocket.BindAddress,
		"workers", cfg.Workers.PoolSize,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Channels
	telemetryChan := make(chan *protocol.TelemetryEvent, 1000)
	updateChan := make(chan drone.StateUpdate, 256)

	// Core components
	droneManager := drone.NewManager(cfg.Drone, updateChan, logger)
	hub := pubsub.NewHub(cfg.PubSub, telemetryChan, logger)
	udpListener := ingest.NewUDPListener(cfg.UDP, cfg.Workers, cfg.Drone, telemetryChan, logger)
	wsServer := broadcast.NewWebSocketServer(cfg.WebSocket, droneManager, hub, logger)

	// Start components in dependency order
	droneManager.Start(ctx)
	hub.Start(ctx)

	go eventProcessor(ctx, telemetryChan, droneManager, logger)

	go func() {
		if err := wsServer.Start(ctx); err != nil {
			logger.Error("WebSocket server error", "error", err)
		}
	}()

	go func() {
		if err := udpListener.Start(ctx); err != nil {
			logger.Error("UDP listener error", "error", err)
			cancel()
		}
	}()

	// Prometheus metrics server
	if cfg.Metrics.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())

		// Optional pprof endpoints on metrics server
		if cfg.Debug.PprofEnabled {
			metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
			metricsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			metricsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			metricsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			logger.Info("pprof endpoints enabled", "address", cfg.Metrics.BindAddress)
		}

		metricsServer := &http.Server{
			Addr:    cfg.Metrics.BindAddress,
			Handler: metricsMux,
		}
		go func() {
			logger.Info("metrics server starting", "address", cfg.Metrics.BindAddress)
			if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
				logger.Error("metrics server error", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = metricsServer.Shutdown(shutCtx)
		}()
	}

	// Register expanded health handler
	registerExpandedHealth(wsServer, droneManager, hub, udpListener, telemetryChan)

	printBanner(logger, cfg)

	sig := <-sigChan
	logger.Info("received shutdown signal", "signal", sig)

	// Graceful shutdown: stop UDP -> drain workers -> flush WS -> close WS -> stop HTTP
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer shutdownCancel()

	<-shutdownCtx.Done()

	wsServer.Stop()
	hub.Stop()
	droneManager.Stop()

	printStats(logger, droneManager, hub, udpListener)
	logger.Info("server shutdown complete")
}

// registerExpandedHealth replaces the basic health handler with an expanded one.
func registerExpandedHealth(
	ws *broadcast.WebSocketServer,
	mgr *drone.Manager,
	hub *pubsub.Hub,
	listener *ingest.UDPListener,
	telemetryChan chan *protocol.TelemetryEvent,
) {
	ws.RegisterHandler("/api/health", func(w http.ResponseWriter, r *http.Request) {
		stats := mgr.Stats()
		hubStats := hub.Stats()
		listenerStats := listener.Stats()

		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		resp := expandedHealthResponse{
			Status:    "ok",
			Version:   version,
			Timestamp: time.Now().UnixMilli(),
			Uptime:    time.Since(startTime).Seconds(),
			Components: componentHealth{
				UDP: udpHealth{
					PacketsReceived: listenerStats.PacketsReceived,
					PacketsDropped:  listenerStats.PacketsDropped,
					ParseErrors:     listenerStats.ParseErrors,
				},
				Drones: droneHealth{
					Total:     stats.TotalDrones,
					Connected: stats.ConnectedDrones,
					Armed:     stats.ArmedDrones,
					Messages:  stats.TotalMessages,
				},
				PubSub: pubSubHealth{
					Subscribers:     hubStats.Subscribers,
					EventsReceived:  hubStats.EventsReceived,
					EventsBroadcast: hubStats.EventsBroadcast,
					EventsDropped:   hubStats.EventsDropped,
				},
				WebSocket: wsHealth{
					Clients: ws.ClientCount(),
				},
			},
			System: systemHealth{
				Goroutines:     runtime.NumGoroutine(),
				HeapAllocMB:    float64(memStats.HeapAlloc) / (1024 * 1024),
				TelemetryChanUtil: float64(len(telemetryChan)) / float64(cap(telemetryChan)),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

var startTime = time.Now()

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

	opts := &slog.HandlerOptions{Level: logLevel}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// Expanded health response types

type expandedHealthResponse struct {
	Status     string          `json:"status"`
	Version    string          `json:"version"`
	Timestamp  int64           `json:"timestamp"`
	Uptime     float64         `json:"uptime_seconds"`
	Components componentHealth `json:"components"`
	System     systemHealth    `json:"system"`
}

type componentHealth struct {
	UDP       udpHealth    `json:"udp"`
	Drones    droneHealth  `json:"drones"`
	PubSub    pubSubHealth `json:"pubsub"`
	WebSocket wsHealth     `json:"websocket"`
}

type udpHealth struct {
	PacketsReceived uint64 `json:"packets_received"`
	PacketsDropped  uint64 `json:"packets_dropped"`
	ParseErrors     uint64 `json:"parse_errors"`
}

type droneHealth struct {
	Total     int    `json:"total"`
	Connected int    `json:"connected"`
	Armed     int    `json:"armed"`
	Messages  uint64 `json:"messages"`
}

type pubSubHealth struct {
	Subscribers     int    `json:"subscribers"`
	EventsReceived  uint64 `json:"events_received"`
	EventsBroadcast uint64 `json:"events_broadcast"`
	EventsDropped   uint64 `json:"events_dropped"`
}

type wsHealth struct {
	Clients int `json:"clients"`
}

type systemHealth struct {
	Goroutines        int     `json:"goroutines"`
	HeapAllocMB       float64 `json:"heap_alloc_mb"`
	TelemetryChanUtil float64 `json:"telemetry_chan_utilization"`
}

func printBanner(logger *slog.Logger, cfg config.Config) {
	logger.Info("=== DRONE TELEMETRY AGGREGATOR ===")
	logger.Info("server ready",
		"udp_ingest", cfg.UDP.BindAddress,
		"http_api", cfg.WebSocket.BindAddress,
	)
	if cfg.Metrics.Enabled {
		logger.Info("metrics endpoint", "address", cfg.Metrics.BindAddress+"/metrics")
	}
}

func printStats(
	logger *slog.Logger,
	manager *drone.Manager,
	hub *pubsub.Hub,
	listener *ingest.UDPListener,
) {
	managerStats := manager.Stats()
	hubStats := hub.Stats()
	listenerStats := listener.Stats()

	logger.Info("final statistics",
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
