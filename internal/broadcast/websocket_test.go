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

	"github.com/coder/websocket"

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
	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
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

func TestMatchesDrone(t *testing.T) {
	c := &wsClient{
		id:       1,
		messages: make(chan []byte, 1),
	}

	// No filter: matches everything
	if !c.matchesDrone(1) {
		t.Error("no filter should match all drones")
	}
	if !c.matchesDrone(99) {
		t.Error("no filter should match all drones")
	}

	// With filter
	c.mu.Lock()
	c.droneIDs = map[uint8]bool{1: true, 5: true}
	c.mu.Unlock()

	if !c.matchesDrone(1) {
		t.Error("drone 1 should match filter")
	}
	if !c.matchesDrone(5) {
		t.Error("drone 5 should match filter")
	}
	if c.matchesDrone(2) {
		t.Error("drone 2 should not match filter")
	}
}

func TestBroadcastUpdates_WithFilter(t *testing.T) {
	ws, mgr, hub, _ := testSetup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	// Register two drones
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID: protocol.DroneID{SystemID: 1, ComponentID: 1}, Timestamp: time.Now(),
		Payload: &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor},
	})
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID: protocol.DroneID{SystemID: 2, ComponentID: 1}, Timestamp: time.Now(),
		Payload: &protocol.Heartbeat{Type: protocol.MAVTypeFixedWing},
	})

	// Add a client with filter for drone 1 only
	filteredClient := &wsClient{
		id:       1,
		messages: make(chan []byte, 10),
		droneIDs: map[uint8]bool{1: true},
	}
	// Add an unfiltered client
	unfilteredClient := &wsClient{
		id:       2,
		messages: make(chan []byte, 10),
	}

	ws.mu.Lock()
	ws.clients[1] = filteredClient
	ws.clients[2] = unfilteredClient
	ws.mu.Unlock()

	ws.broadcastUpdates([]*protocol.TelemetryEvent{{}})

	// Check filtered client got only drone 1
	select {
	case data := <-filteredClient.messages:
		var msg BroadcastMessage
		json.Unmarshal(data, &msg)
		if len(msg.Drones) != 1 {
			t.Errorf("filtered client got %d drones, want 1", len(msg.Drones))
		}
		if len(msg.Drones) > 0 && msg.Drones[0].SystemID != 1 {
			t.Errorf("filtered client got drone %d, want 1", msg.Drones[0].SystemID)
		}
	default:
		t.Error("filtered client got no message")
	}

	// Check unfiltered client got both drones
	select {
	case data := <-unfilteredClient.messages:
		var msg BroadcastMessage
		json.Unmarshal(data, &msg)
		if len(msg.Drones) != 2 {
			t.Errorf("unfiltered client got %d drones, want 2", len(msg.Drones))
		}
	default:
		t.Error("unfiltered client got no message")
	}

	hub.Stop()
	mgr.Stop()
}

func TestBroadcastUpdates_SlowClientDrop(t *testing.T) {
	ws, mgr, hub, _ := testSetup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID: protocol.DroneID{SystemID: 1, ComponentID: 1}, Timestamp: time.Now(),
		Payload: &protocol.Heartbeat{},
	})

	// Client with full message buffer (size 0 means immediately full)
	slowClient := &wsClient{
		id:       1,
		messages: make(chan []byte), // unbuffered = always blocks
	}
	ws.mu.Lock()
	ws.clients[1] = slowClient
	ws.mu.Unlock()

	// Should not block or panic
	ws.broadcastUpdates([]*protocol.TelemetryEvent{{}})

	hub.Stop()
	mgr.Stop()
}

func TestWebSocket_SubscribeFilter(t *testing.T) {
	ws, mgr, hub, telemetryChan := testSetup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	hub.Start(ctx)

	srv := httptest.NewServer(ws.mux)
	defer srv.Close()

	ws.wg.Add(1)
	go ws.broadcastLoop(ctx)

	// Register drones
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID: protocol.DroneID{SystemID: 1, ComponentID: 1}, Timestamp: time.Now(),
		Payload: &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor},
	})
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID: protocol.DroneID{SystemID: 2, ComponentID: 1}, Timestamp: time.Now(),
		Payload: &protocol.Heartbeat{Type: protocol.MAVTypeFixedWing},
	})

	wsURL := "ws" + srv.URL[4:] + "/ws"
	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	// Send subscribe message to filter to drone 1 only
	subMsg := SubscribeMessage{DroneIDs: []uint8{1}}
	subData, _ := json.Marshal(subMsg)
	err = c.Write(ctx, websocket.MessageText, subData)
	if err != nil {
		t.Fatalf("websocket write subscribe: %v", err)
	}

	// Give time for subscribe to be processed
	time.Sleep(100 * time.Millisecond)

	// Push a telemetry event to trigger broadcast
	telemetryChan <- &protocol.TelemetryEvent{
		DroneID: protocol.DroneID{SystemID: 1, ComponentID: 1}, Timestamp: time.Now(),
		Payload: &protocol.Heartbeat{},
	}

	time.Sleep(200 * time.Millisecond)

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, data, err := c.Read(readCtx)
	if err != nil {
		t.Fatalf("websocket read: %v", err)
	}

	var msg BroadcastMessage
	json.Unmarshal(data, &msg)
	if len(msg.Drones) != 1 {
		t.Errorf("filtered WS got %d drones, want 1", len(msg.Drones))
	}
	if len(msg.Drones) > 0 && msg.Drones[0].SystemID != 1 {
		t.Errorf("filtered WS got drone %d, want 1", msg.Drones[0].SystemID)
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
	c1, resp1, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	if resp1 != nil && resp1.Body != nil {
		defer resp1.Body.Close()
	}
	defer c1.Close(websocket.StatusNormalClosure, "")

	c2, resp2, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	if resp2 != nil && resp2.Body != nil {
		defer resp2.Body.Close()
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
