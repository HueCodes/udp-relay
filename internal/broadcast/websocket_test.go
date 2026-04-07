package broadcast

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/internal/pubsub"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

func testSetup() (*WebSocketServer, *drone.Manager, *pubsub.Hub, chan *protocol.TelemetryEvent) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	telemetryChan := make(chan *protocol.TelemetryEvent, 100)
	updateChan := make(chan drone.StateUpdate, 100)

	droneCfg := config.DroneConfig{
		StaleCheckInterval: 10 * time.Second,
		StaleThreshold:     30 * time.Second,
	}
	mgr := drone.NewManager(droneCfg, updateChan, logger)

	hubCfg := config.PubSubConfig{
		SubscriberBufferSize: 256,
		DropOnSlowSubscriber: true,
	}
	hub := pubsub.NewHub(hubCfg, telemetryChan, logger)

	wsCfg := config.WebSocketConfig{
		BindAddress:       ":0",
		BroadcastInterval: 50 * time.Millisecond,
		WriteTimeout:      5 * time.Second,
		MaxMessageSize:    4096,
		MaxClients:        10,
	}
	ws := NewWebSocketServer(wsCfg, mgr, hub, logger)

	return ws, mgr, hub, telemetryChan
}

func TestHandleDroneList(t *testing.T) {
	ws, mgr, hub, _ := testSetup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	// Register a drone
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: 1, ComponentID: 1},
		Timestamp: time.Now(),
		Payload:   &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/drones", nil)
	rec := httptest.NewRecorder()
	ws.handleDroneList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp DroneListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("Count = %d, want 1", resp.Count)
	}
	if len(resp.Drones) != 1 {
		t.Fatalf("len(Drones) = %d, want 1", len(resp.Drones))
	}
	if resp.Drones[0].SystemID != 1 {
		t.Errorf("SystemID = %d, want 1", resp.Drones[0].SystemID)
	}

	hub.Stop()
	mgr.Stop()
}

func TestHandleDroneList_MethodNotAllowed(t *testing.T) {
	ws, _, _, _ := testSetup()

	req := httptest.NewRequest(http.MethodPost, "/api/drones", nil)
	rec := httptest.NewRecorder()
	ws.handleDroneList(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleHealth(t *testing.T) {
	ws, mgr, hub, _ := testSetup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	ws.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("Status = %q, want ok", resp.Status)
	}

	hub.Stop()
	mgr.Stop()
}

func TestRegisterHandler(t *testing.T) {
	ws, _, _, _ := testSetup()

	ws.RegisterHandler("/custom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	srv := httptest.NewServer(ws.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/custom")
	if err != nil {
		t.Fatalf("GET /custom: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
}

func TestClientCount(t *testing.T) {
	ws, _, _, _ := testSetup()
	if ws.ClientCount() != 0 {
		t.Fatalf("ClientCount = %d, want 0", ws.ClientCount())
	}
}

func TestWebSocket_ConnectDisconnect(t *testing.T) {
	ws, mgr, hub, telemetryChan := testSetup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	srv := httptest.NewServer(ws.mux)
	defer srv.Close()

	// Start broadcast loop in background
	ws.wg.Add(1)
	go ws.broadcastLoop(ctx)

	// Connect via WebSocket
	wsURL := "ws" + srv.URL[4:] + "/ws"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}

	// Register a drone and push a telemetry event
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: 1, ComponentID: 1},
		Timestamp: time.Now(),
		Payload:   &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor},
	})
	telemetryChan <- &protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: 1, ComponentID: 1},
		Timestamp: time.Now(),
		Payload:   &protocol.Heartbeat{},
	}

	// Wait for broadcast tick
	time.Sleep(200 * time.Millisecond)

	// Read a message
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, data, err := c.Read(readCtx)
	if err != nil {
		t.Fatalf("websocket read: %v", err)
	}

	var msg BroadcastMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("failed to parse WS message: %v", err)
	}
	if msg.Type != "state_update" {
		t.Errorf("Type = %q, want state_update", msg.Type)
	}

	c.Close(websocket.StatusNormalClosure, "")
	hub.Stop()
	mgr.Stop()
}

func TestWebSocket_MaxClients(t *testing.T) {
	ws, mgr, hub, _ := testSetup()
	// Set max clients to 2
	ws.cfg.MaxClients = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	srv := httptest.NewServer(ws.mux)
	defer srv.Close()

	ws.wg.Add(1)
	go ws.broadcastLoop(ctx)

	wsURL := "ws" + srv.URL[4:] + "/ws"

	// Connect 2 clients
	c1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "")

	c2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	// Wait for connection handlers to register
	time.Sleep(100 * time.Millisecond)

	// 3rd should be rejected
	resp, err := http.Get(srv.URL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("3rd client status = %d, want 503", resp.StatusCode)
	}

	hub.Stop()
	mgr.Stop()
}
