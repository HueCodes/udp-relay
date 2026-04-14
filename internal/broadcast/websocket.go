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

	"github.com/coder/websocket"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/internal/pubsub"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// WebSocketServer handles WebSocket connections for real-time telemetry.
type WebSocketServer struct {
	cfg          config.WebSocketConfig
	logger       *slog.Logger
	droneManager *drone.Manager
	hub          *pubsub.Hub
	subscription *pubsub.Subscriber

	// HTTP server
	server *http.Server
	mux    *http.ServeMux

	// Connected WebSocket clients
	mu      sync.RWMutex
	clients map[uint64]*wsClient

	nextID atomic.Uint64
	done   chan struct{}
	wg     sync.WaitGroup
}

// SubscribeMessage is sent by the client to filter updates.
type SubscribeMessage struct {
	// DroneIDs filters to specific drone system IDs (empty = all)
	DroneIDs []uint8 `json:"drone_ids,omitempty"`
	// EventTypes filters to specific update types (empty = all)
	EventTypes []string `json:"event_types,omitempty"`
}

// wsClient represents a connected WebSocket client.
type wsClient struct {
	id     uint64
	conn   *websocket.Conn
	cancel context.CancelFunc

	// Filtering
	mu         sync.RWMutex
	droneIDs   map[uint8]bool
	eventTypes map[string]bool

	// Outbound message queue
	messages chan []byte
}

func (c *wsClient) matchesDrone(id uint8) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.droneIDs) == 0 {
		return true
	}
	return c.droneIDs[id]
}

func (c *wsClient) hasFilter() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.droneIDs) > 0
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
		hub:          hub,
		clients:      make(map[uint64]*wsClient),
		done:         make(chan struct{}),
		mux:          http.NewServeMux(),
	}

	// Subscribe to telemetry events
	ws.subscription = hub.Subscribe("websocket_broadcaster")

	// Register HTTP handlers
	ws.mux.HandleFunc("/ws", ws.handleWebSocket)
	ws.mux.HandleFunc("/api/drones", ws.handleDroneList)
	ws.mux.HandleFunc("/api/health", ws.handleHealth)
	ws.registerExportHandlers()

	return ws
}

// RegisterHandler adds an HTTP handler to the server's mux.
// Must be called before Start.
func (ws *WebSocketServer) RegisterHandler(pattern string, handler http.HandlerFunc) {
	ws.mux.HandleFunc(pattern, handler)
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

	// Close all client connections
	ws.mu.Lock()
	for _, c := range ws.clients {
		c.cancel()
	}
	ws.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ws.server != nil {
		_ = ws.server.Shutdown(ctx)
	}

	ws.wg.Wait()
	ws.logger.Info("WebSocket server stopped")
}

// ClientCount returns the number of connected WebSocket clients.
func (ws *WebSocketServer) ClientCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.clients)
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

// broadcastUpdates sends batched updates to all connected WebSocket clients.
func (ws *WebSocketServer) broadcastUpdates(_ []*protocol.TelemetryEvent) {
	summaries := ws.droneManager.GetAllSummaries()

	msg := BroadcastMessage{
		Type:      "state_update",
		Timestamp: time.Now().UnixMilli(),
		Drones:    summaries,
	}

	fullData, err := json.Marshal(msg)
	if err != nil {
		ws.logger.Error("failed to marshal broadcast", "error", err)
		return
	}

	ws.mu.RLock()
	defer ws.mu.RUnlock()

	for _, c := range ws.clients {
		// Check if client has drone filters
		if c.hasFilter() {
			filtered := make([]drone.Summary, 0, len(summaries))
			for _, s := range summaries {
				if c.matchesDrone(s.SystemID) {
					filtered = append(filtered, s)
				}
			}
			filteredMsg := BroadcastMessage{
				Type:      "state_update",
				Timestamp: msg.Timestamp,
				Drones:    filtered,
			}
			data, err := json.Marshal(filteredMsg)
			if err != nil {
				continue
			}
			select {
			case c.messages <- data:
			default:
			}
		} else {
			select {
			case c.messages <- fullData:
			default:
			}
		}
	}
}

// handleWebSocket upgrades HTTP to WebSocket and manages the client lifecycle.
func (ws *WebSocketServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check max clients
	ws.mu.RLock()
	count := len(ws.clients)
	ws.mu.RUnlock()
	if ws.cfg.MaxClients > 0 && count >= ws.cfg.MaxClients {
		http.Error(w, "max clients reached", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		ws.logger.Warn("websocket accept failed", "error", err)
		return
	}

	clientCtx, cancel := context.WithCancel(r.Context())
	id := ws.nextID.Add(1)

	c := &wsClient{
		id:       id,
		conn:     conn,
		cancel:   cancel,
		messages: make(chan []byte, 64),
	}

	ws.mu.Lock()
	ws.clients[id] = c
	ws.mu.Unlock()

	ws.logger.Info("websocket client connected",
		"client_id", id,
		"remote", r.RemoteAddr,
		"total_clients", ws.ClientCount())

	// Read loop: handle incoming subscribe messages
	ws.wg.Add(1)
	go ws.clientReadLoop(clientCtx, c)

	// Write loop: send broadcasts to client
	ws.wg.Add(1)
	go ws.clientWriteLoop(clientCtx, c)

	// Wait for client disconnect
	<-clientCtx.Done()

	// Cleanup
	ws.mu.Lock()
	delete(ws.clients, id)
	ws.mu.Unlock()

	_ = conn.Close(websocket.StatusNormalClosure, "")
	ws.logger.Info("websocket client disconnected",
		"client_id", id,
		"total_clients", ws.ClientCount())
}

// clientReadLoop handles incoming messages from a WebSocket client.
func (ws *WebSocketServer) clientReadLoop(ctx context.Context, c *wsClient) {
	defer ws.wg.Done()
	defer c.cancel()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}

		var sub SubscribeMessage
		if err := json.Unmarshal(data, &sub); err != nil {
			ws.logger.Debug("invalid subscribe message", "client_id", c.id, "error", err)
			continue
		}

		c.mu.Lock()
		if len(sub.DroneIDs) > 0 {
			c.droneIDs = make(map[uint8]bool, len(sub.DroneIDs))
			for _, id := range sub.DroneIDs {
				c.droneIDs[id] = true
			}
		} else {
			c.droneIDs = nil
		}
		if len(sub.EventTypes) > 0 {
			c.eventTypes = make(map[string]bool, len(sub.EventTypes))
			for _, t := range sub.EventTypes {
				c.eventTypes[t] = true
			}
		} else {
			c.eventTypes = nil
		}
		c.mu.Unlock()

		ws.logger.Debug("client updated subscription",
			"client_id", c.id,
			"drone_ids", sub.DroneIDs,
			"event_types", sub.EventTypes)
	}
}

// clientWriteLoop sends queued messages to a WebSocket client.
func (ws *WebSocketServer) clientWriteLoop(ctx context.Context, c *wsClient) {
	defer ws.wg.Done()
	defer c.cancel()

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-c.messages:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, ws.cfg.WriteTimeout)
			err := c.conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}

		case <-pingTicker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
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
	_ = json.NewEncoder(w).Encode(response)
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
	_ = json.NewEncoder(w).Encode(response)
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
