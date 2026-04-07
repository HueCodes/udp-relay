// Package broadcast provides WebSocket broadcasting of drone telemetry.
package broadcast

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/internal/pubsub"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// WebSocketServer handles WebSocket connections for real-time telemetry.
// This is a placeholder implementation - a production version would use
// gorilla/websocket or nhooyr.io/websocket.
type WebSocketServer struct {
	cfg          config.WebSocketConfig
	logger       *slog.Logger
	droneManager *drone.Manager
	subscription *pubsub.Subscriber

	// HTTP server
	server *http.Server
	mux    *http.ServeMux

	// Connected clients (placeholder - would use proper WebSocket connections)
	mu      sync.RWMutex
	clients map[uint64]*client

	nextID atomic.Uint64
	done   chan struct{}
	wg     sync.WaitGroup
}

// client represents a connected WebSocket client (placeholder).
type client struct {
	id       uint64
	messages chan []byte
}

// NewWebSocketServer creates a new WebSocket server.
func NewWebSocketServer(
	cfg config.WebSocketConfig,
	droneManager *drone.Manager,
	hub *pubsub.Hub,
	logger *slog.Logger,
) *WebSocketServer {
	ws := &WebSocketServer{
		cfg:          cfg,
		logger:       logger.With("component", "websocket"),
		droneManager: droneManager,
		clients:      make(map[uint64]*client),
		done:         make(chan struct{}),
		mux:          http.NewServeMux(),
	}

	// Subscribe to telemetry events
	ws.subscription = hub.Subscribe("websocket_broadcaster")

	// Register HTTP handlers
	ws.mux.HandleFunc("/ws", ws.handleWebSocket)
	ws.mux.HandleFunc("/api/drones", ws.handleDroneList)
	ws.mux.HandleFunc("/api/health", ws.handleHealth)

	return ws
}

// Start begins the WebSocket server.
func (ws *WebSocketServer) Start(ctx context.Context) error {
	ws.server = &http.Server{
		Addr:         ws.cfg.BindAddress,
		Handler:      ws.mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: ws.cfg.WriteTimeout,
	}

	// Start broadcast loop
	ws.wg.Add(1)
	go ws.broadcastLoop(ctx)

	// Start HTTP server
	ws.wg.Add(1)
	go func() {
		defer ws.wg.Done()
		ws.logger.Info("WebSocket server starting", "address", ws.cfg.BindAddress)
		if err := ws.server.ListenAndServe(); err != http.ErrServerClosed {
			ws.logger.Error("HTTP server error", "error", err)
		}
	}()

	// Wait for shutdown
	<-ctx.Done()
	ws.Stop()

	return nil
}

// Stop gracefully shuts down the server.
func (ws *WebSocketServer) Stop() {
	close(ws.done)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ws.server != nil {
		ws.server.Shutdown(ctx)
	}

	ws.wg.Wait()
	ws.logger.Info("WebSocket server stopped")
}

// broadcastLoop processes telemetry events and broadcasts to clients.
func (ws *WebSocketServer) broadcastLoop(ctx context.Context) {
	defer ws.wg.Done()

	for {
		if !ws.runBroadcastLoop(ctx) {
			return
		}
	}
}

func (ws *WebSocketServer) runBroadcastLoop(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			ws.logger.Error("broadcast loop panicked, restarting",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			panicked = true
		}
	}()

	ticker := time.NewTicker(ws.cfg.BroadcastInterval)
	defer ticker.Stop()

	var pendingUpdates []*protocol.TelemetryEvent

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ws.done:
			return false

		case event, ok := <-ws.subscription.Events:
			if !ok {
				return false
			}
			pendingUpdates = append(pendingUpdates, event)

		case <-ticker.C:
			if len(pendingUpdates) > 0 {
				ws.broadcastUpdates(pendingUpdates)
				pendingUpdates = pendingUpdates[:0]
			}
		}
	}
}

// broadcastUpdates sends batched updates to all clients.
func (ws *WebSocketServer) broadcastUpdates(events []*protocol.TelemetryEvent) {
	// Get current drone summaries
	summaries := ws.droneManager.GetAllSummaries()

	msg := BroadcastMessage{
		Type:      "state_update",
		Timestamp: time.Now().UnixMilli(),
		Drones:    summaries,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		ws.logger.Error("failed to marshal broadcast", "error", err)
		return
	}

	ws.mu.RLock()
	defer ws.mu.RUnlock()

	for _, c := range ws.clients {
		select {
		case c.messages <- data:
		default:
			// Client too slow, drop message
		}
	}
}

// handleWebSocket handles WebSocket upgrade requests.
// This is a placeholder - actual implementation would use a WebSocket library.
func (ws *WebSocketServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Placeholder: In production, this would:
	// 1. Upgrade connection to WebSocket
	// 2. Register client
	// 3. Handle incoming messages
	// 4. Send outgoing broadcasts

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	json.NewEncoder(w).Encode(map[string]string{
		"error":   "WebSocket not implemented",
		"message": "Use /api/drones for REST polling or implement WebSocket upgrade",
	})
}

// handleDroneList returns the current state of all drones.
func (ws *WebSocketServer) handleDroneList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	summaries := ws.droneManager.GetAllSummaries()

	response := DroneListResponse{
		Timestamp: time.Now().UnixMilli(),
		Count:     len(summaries),
		Drones:    summaries,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(response)
}

// handleHealth returns server health status.
func (ws *WebSocketServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	stats := ws.droneManager.Stats()

	response := HealthResponse{
		Status:          "ok",
		Timestamp:       time.Now().UnixMilli(),
		ConnectedDrones: stats.ConnectedDrones,
		TotalDrones:     stats.TotalDrones,
		TotalMessages:   stats.TotalMessages,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// BroadcastMessage is sent to WebSocket clients.
type BroadcastMessage struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	Drones    []drone.Summary `json:"drones"`
}

// DroneListResponse is the REST API response for drone list.
type DroneListResponse struct {
	Timestamp int64           `json:"timestamp"`
	Count     int             `json:"count"`
	Drones    []drone.Summary `json:"drones"`
}

// HealthResponse is the health check response.
type HealthResponse struct {
	Status          string `json:"status"`
	Timestamp       int64  `json:"timestamp"`
	ConnectedDrones int    `json:"connected_drones"`
	TotalDrones     int    `json:"total_drones"`
	TotalMessages   uint64 `json:"total_messages"`
}
