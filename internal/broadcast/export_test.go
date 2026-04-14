package broadcast

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hugh/go-drone-server/internal/config"
	"github.com/hugh/go-drone-server/internal/drone"
	"github.com/hugh/go-drone-server/internal/pubsub"
	"github.com/hugh/go-drone-server/pkg/protocol"
)

func testServer(t *testing.T) (*WebSocketServer, *drone.Manager, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Drone.HistorySize = 100

	telemetryChan := make(chan *protocol.TelemetryEvent, 100)
	updateChan := make(chan drone.StateUpdate, 100)
	mgr := drone.NewManager(cfg.Drone, updateChan, logger)
	hub := pubsub.NewHub(cfg.PubSub, telemetryChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	hub.Start(ctx)

	ws := NewWebSocketServer(cfg.WebSocket, mgr, hub, logger)

	// Drain updates
	go func() {
		for range updateChan {
		}
	}()

	cleanup := func() {
		cancel()
		hub.Stop()
		mgr.Stop()
	}

	return ws, mgr, cleanup
}

func seedDrone(mgr *drone.Manager, id uint8) {
	now := time.Now()
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: id, ComponentID: 1},
		Timestamp: now,
		Payload:   &protocol.Heartbeat{Type: protocol.MAVTypeQuadrotor, Armed: true},
	})
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: id, ComponentID: 1},
		Timestamp: now,
		Payload: &protocol.GPSPosition{
			Latitude: 37.7749 + float64(id)*0.001, Longitude: -122.4194,
			Altitude: 50, Heading: 270,
		},
	})
	mgr.ProcessEvent(&protocol.TelemetryEvent{
		DroneID:   protocol.DroneID{SystemID: id, ComponentID: 1},
		Timestamp: now,
		Payload:   &protocol.BatteryStatus{Remaining: 72, VoltageTotal: 16.2},
	})
}

func TestExportGeoJSON(t *testing.T) {
	ws, mgr, cleanup := testServer(t)
	defer cleanup()

	seedDrone(mgr, 1)
	seedDrone(mgr, 2)

	req := httptest.NewRequest("GET", "/api/drones/export?format=geojson", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/geo+json" {
		t.Errorf("expected geo+json content type, got %s", ct)
	}

	var fc geoJSONCollection
	if err := json.Unmarshal(w.Body.Bytes(), &fc); err != nil {
		t.Fatalf("failed to parse GeoJSON: %v", err)
	}

	if fc.Type != "FeatureCollection" {
		t.Errorf("expected FeatureCollection, got %s", fc.Type)
	}

	if len(fc.Features) != 2 {
		t.Errorf("expected 2 features, got %d", len(fc.Features))
	}
}

func TestExportKML(t *testing.T) {
	ws, mgr, cleanup := testServer(t)
	defer cleanup()

	seedDrone(mgr, 1)

	req := httptest.NewRequest("GET", "/api/drones/export?format=kml", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "<kml") {
		t.Error("expected KML output")
	}
	if !strings.Contains(body, "Drone 1") {
		t.Error("expected Drone 1 in KML")
	}
}

func TestHistoryEndpoint(t *testing.T) {
	ws, mgr, cleanup := testServer(t)
	defer cleanup()

	// Push multiple GPS updates
	for i := 0; i < 5; i++ {
		mgr.ProcessEvent(&protocol.TelemetryEvent{
			DroneID:   protocol.DroneID{SystemID: 1, ComponentID: 1},
			Timestamp: time.Now(),
			Payload: &protocol.GPSPosition{
				Latitude: 37.7749 + float64(i)*0.0001, Longitude: -122.4194,
				Altitude: float64(50 + i),
			},
		})
	}

	req := httptest.NewRequest("GET", "/api/drones/1/history?last=3", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		SystemID uint8                `json:"system_id"`
		Count    int                  `json:"count"`
		Entries  []drone.HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Count != 3 {
		t.Errorf("expected 3 entries, got %d", resp.Count)
	}
	if resp.SystemID != 1 {
		t.Errorf("expected system_id 1, got %d", resp.SystemID)
	}
}

func TestHistoryNotFound(t *testing.T) {
	ws, _, cleanup := testServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/drones/99/history", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDroneByID(t *testing.T) {
	ws, mgr, cleanup := testServer(t)
	defer cleanup()

	seedDrone(mgr, 5)

	req := httptest.NewRequest("GET", "/api/drones/5", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var summary drone.Summary
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if summary.SystemID != 5 {
		t.Errorf("expected system ID 5, got %d", summary.SystemID)
	}
}

func TestHistoryExportGeoJSON(t *testing.T) {
	ws, mgr, cleanup := testServer(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		mgr.ProcessEvent(&protocol.TelemetryEvent{
			DroneID:   protocol.DroneID{SystemID: 1, ComponentID: 1},
			Timestamp: time.Now(),
			Payload: &protocol.GPSPosition{
				Latitude: 37.7749 + float64(i)*0.0001, Longitude: -122.4194,
				Altitude: float64(50 + i),
			},
		})
	}

	req := httptest.NewRequest("GET", "/api/drones/1/history/export?format=geojson", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var fc geoJSONCollection
	if err := json.Unmarshal(w.Body.Bytes(), &fc); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	if len(fc.Features) != 3 {
		t.Errorf("expected 3 features, got %d", len(fc.Features))
	}
}

func TestExportInvalidFormat(t *testing.T) {
	ws, mgr, cleanup := testServer(t)
	defer cleanup()

	seedDrone(mgr, 1)

	req := httptest.NewRequest("GET", "/api/drones/export?format=csv", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestInvalidDroneID(t *testing.T) {
	ws, _, cleanup := testServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/drones/abc", nil)
	w := httptest.NewRecorder()
	ws.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
