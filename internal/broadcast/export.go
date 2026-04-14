package broadcast

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hugh/go-drone-server/internal/drone"
)

func (ws *WebSocketServer) registerExportHandlers() {
	ws.mux.HandleFunc("/api/drones/export", ws.handleExport)
	ws.mux.HandleFunc("/api/drones/", ws.handleDroneRoutes)
}

// handleExport returns all active drones as GeoJSON or KML.
func (ws *WebSocketServer) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	format := r.URL.Query().Get("format")
	summaries := ws.droneManager.GetAllSummaries()

	switch format {
	case "kml":
		w.Header().Set("Content-Type", "application/vnd.google-earth.kml+xml")
		w.Header().Set("Content-Disposition", "attachment; filename=drones.kml")
		writeKML(w, summaries)
	case "geojson", "":
		w.Header().Set("Content-Type", "application/geo+json")
		writeGeoJSON(w, summaries)
	default:
		http.Error(w, "unsupported format: use geojson or kml", http.StatusBadRequest)
	}
}

// handleDroneRoutes dispatches per-drone sub-routes.
func (ws *WebSocketServer) handleDroneRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse: /api/drones/{id}/history or /api/drones/{id}/history/export
	path := strings.TrimPrefix(r.URL.Path, "/api/drones/")
	parts := strings.Split(path, "/")
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, "drone ID required", http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(parts[0])
	if err != nil || id < 1 || id > 250 {
		http.Error(w, "invalid drone ID", http.StatusBadRequest)
		return
	}
	systemID := uint8(id)

	if len(parts) < 2 {
		// /api/drones/{id} -- return single drone state
		state := ws.droneManager.GetStateBySystemID(systemID)
		if state == nil {
			http.Error(w, "drone not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.ToSummary())
		return
	}

	switch parts[1] {
	case "history":
		if len(parts) >= 3 && parts[2] == "export" {
			ws.handleHistoryExport(w, r, systemID)
		} else {
			ws.handleHistory(w, r, systemID)
		}
	case "stats":
		ws.handleDroneStats(w, r, systemID)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (ws *WebSocketServer) handleHistory(w http.ResponseWriter, r *http.Request, systemID uint8) {
	lastStr := r.URL.Query().Get("last")
	last := 100
	if lastStr != "" {
		if n, err := strconv.Atoi(lastStr); err == nil && n > 0 {
			last = n
		}
	}
	if last > 1000 {
		last = 1000
	}

	entries := ws.droneManager.GetHistory(systemID, last)
	if entries == nil {
		http.Error(w, "drone not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		SystemID uint8                `json:"system_id"`
		Count    int                  `json:"count"`
		Entries  []drone.HistoryEntry `json:"entries"`
	}{
		SystemID: systemID,
		Count:    len(entries),
		Entries:  entries,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (ws *WebSocketServer) handleHistoryExport(w http.ResponseWriter, r *http.Request, systemID uint8) {
	entries := ws.droneManager.GetHistory(systemID, 1000)
	if entries == nil {
		http.Error(w, "drone not found", http.StatusNotFound)
		return
	}

	format := r.URL.Query().Get("format")
	switch format {
	case "kml":
		w.Header().Set("Content-Type", "application/vnd.google-earth.kml+xml")
		writeTrajectoryKML(w, systemID, entries)
	case "geojson", "":
		w.Header().Set("Content-Type", "application/geo+json")
		writeTrajectoryGeoJSON(w, systemID, entries)
	default:
		http.Error(w, "unsupported format", http.StatusBadRequest)
	}
}

func (ws *WebSocketServer) handleDroneStats(w http.ResponseWriter, _ *http.Request, systemID uint8) {
	state := ws.droneManager.GetStateBySystemID(systemID)
	if state == nil {
		http.Error(w, "drone not found", http.StatusNotFound)
		return
	}

	histLen := 0
	if state.History != nil {
		histLen = state.History.Len()
	}

	resp := struct {
		SystemID     uint8   `json:"system_id"`
		Connected    bool    `json:"connected"`
		MessageCount uint64  `json:"message_count"`
		HistorySize  int     `json:"history_entries"`
		UptimeMs     int64   `json:"uptime_ms"`
		Armed        bool    `json:"armed"`
		FlightMode   string  `json:"flight_mode"`
	}{
		SystemID:     systemID,
		Connected:    state.IsConnected,
		MessageCount: state.MessageCount,
		HistorySize:  histLen,
		UptimeMs:     state.LastSeen.Sub(state.FirstSeen).Milliseconds(),
		Armed:        state.IsArmed,
		FlightMode:   state.FlightMode,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// GeoJSON types

type geoJSONCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
}

type geoJSONFeature struct {
	Type       string          `json:"type"`
	Geometry   geoJSONGeometry `json:"geometry"`
	Properties map[string]any  `json:"properties"`
}

type geoJSONGeometry struct {
	Type        string  `json:"type"`
	Coordinates any     `json:"coordinates"`
}

func writeGeoJSON(w http.ResponseWriter, summaries []drone.Summary) {
	fc := geoJSONCollection{
		Type:     "FeatureCollection",
		Features: make([]geoJSONFeature, 0, len(summaries)),
	}

	for _, s := range summaries {
		if s.Latitude == nil || s.Longitude == nil {
			continue
		}

		coords := []float64{*s.Longitude, *s.Latitude}
		if s.Altitude != nil {
			coords = append(coords, *s.Altitude)
		}

		props := map[string]any{
			"system_id":   s.SystemID,
			"connected":   s.IsConnected,
			"armed":       s.IsArmed,
			"flight_mode": s.FlightMode,
		}
		if s.Heading != nil {
			props["heading"] = *s.Heading
		}
		if s.BatteryPercent != nil {
			props["battery_pct"] = *s.BatteryPercent
		}
		if s.VehicleType != "" {
			props["vehicle_type"] = s.VehicleType
		}

		fc.Features = append(fc.Features, geoJSONFeature{
			Type:       "Feature",
			Geometry:   geoJSONGeometry{Type: "Point", Coordinates: coords},
			Properties: props,
		})
	}

	_ = json.NewEncoder(w).Encode(fc)
}

func writeTrajectoryGeoJSON(w http.ResponseWriter, systemID uint8, entries []drone.HistoryEntry) {
	fc := geoJSONCollection{
		Type:     "FeatureCollection",
		Features: make([]geoJSONFeature, 0, len(entries)),
	}

	for _, e := range entries {
		fc.Features = append(fc.Features, geoJSONFeature{
			Type: "Feature",
			Geometry: geoJSONGeometry{
				Type:        "Point",
				Coordinates: []float64{e.Lon, e.Lat, e.Alt},
			},
			Properties: map[string]any{
				"system_id":   systemID,
				"timestamp":   e.Timestamp.UnixMilli(),
				"heading":     e.Heading,
				"battery_pct": e.Battery,
				"armed":       e.Armed,
				"flight_mode": e.Mode,
			},
		})
	}

	_ = json.NewEncoder(w).Encode(fc)
}

// KML types

type kmlDoc struct {
	XMLName xml.Name    `xml:"kml"`
	XMLNS   string      `xml:"xmlns,attr"`
	Doc     kmlDocument `xml:"Document"`
}

type kmlDocument struct {
	Name       string         `xml:"name"`
	Placemarks []kmlPlacemark `xml:"Placemark"`
}

type kmlPlacemark struct {
	Name        string   `xml:"name"`
	Description string   `xml:"description"`
	Point       kmlPoint `xml:"Point"`
}

type kmlPoint struct {
	Coordinates string `xml:"coordinates"`
}

func writeKML(w http.ResponseWriter, summaries []drone.Summary) {
	doc := kmlDoc{
		XMLNS: "http://www.opengis.net/kml/2.2",
		Doc: kmlDocument{
			Name: "Active Drones - " + time.Now().Format(time.RFC3339),
		},
	}

	for _, s := range summaries {
		if s.Latitude == nil || s.Longitude == nil {
			continue
		}
		alt := 0.0
		if s.Altitude != nil {
			alt = *s.Altitude
		}

		desc := fmt.Sprintf("Connected: %v, Armed: %v, Mode: %s",
			s.IsConnected, s.IsArmed, s.FlightMode)
		if s.BatteryPercent != nil {
			desc += fmt.Sprintf(", Battery: %d%%", *s.BatteryPercent)
		}

		doc.Doc.Placemarks = append(doc.Doc.Placemarks, kmlPlacemark{
			Name:        fmt.Sprintf("Drone %d", s.SystemID),
			Description: desc,
			Point: kmlPoint{
				Coordinates: fmt.Sprintf("%.7f,%.7f,%.1f", *s.Longitude, *s.Latitude, alt),
			},
		})
	}

	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(doc)
}

func writeTrajectoryKML(w http.ResponseWriter, systemID uint8, entries []drone.HistoryEntry) {
	doc := kmlDoc{
		XMLNS: "http://www.opengis.net/kml/2.2",
		Doc: kmlDocument{
			Name: fmt.Sprintf("Drone %d Trajectory", systemID),
		},
	}

	for i, e := range entries {
		doc.Doc.Placemarks = append(doc.Doc.Placemarks, kmlPlacemark{
			Name: fmt.Sprintf("Point %d", i+1),
			Description: fmt.Sprintf("Time: %s, Alt: %.1fm, Battery: %d%%",
				e.Timestamp.Format(time.RFC3339), e.Alt, e.Battery),
			Point: kmlPoint{
				Coordinates: fmt.Sprintf("%.7f,%.7f,%.1f", e.Lon, e.Lat, e.Alt),
			},
		})
	}

	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(doc)
}
