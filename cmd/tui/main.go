// Package main provides a terminal UI dashboard for monitoring drone telemetry.
// It connects to the server's WebSocket endpoint and displays a live table of
// all drones with color-coded battery levels, flight modes, and connection status.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/coder/websocket"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("252"))

	cellStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("250"))

	connectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	disconnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	armedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	disarmedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	battGoodStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	battWarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))
	battCritStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))

	statusBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

var colWidths = []int{5, 10, 7, 12, 22, 8, 8, 12, 10}

type wsMsg struct {
	Type      string         `json:"type"`
	Timestamp int64          `json:"timestamp"`
	Drones    []droneSummary `json:"drones"`
}

type droneSummary struct {
	SystemID    uint8    `json:"system_id"`
	ComponentID uint8    `json:"component_id"`
	Connected   bool     `json:"connected"`
	Armed       bool     `json:"armed"`
	FlightMode  string   `json:"flight_mode,omitempty"`
	VehicleType string   `json:"vehicle_type,omitempty"`
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	Alt         *float64 `json:"alt,omitempty"`
	Heading     *float64 `json:"heading,omitempty"`
	BatteryPct  *int8    `json:"battery_pct,omitempty"`
	BatteryV    *float64 `json:"battery_v,omitempty"`
	LastSeenMs  int64    `json:"last_seen_ms"`
}

type wsUpdateMsg wsMsg
type wsErrorMsg struct{ err error }
type tickMsg time.Time
type reconnectMsg struct{}

type model struct {
	serverAddr  string
	drones      []droneSummary
	connected   bool
	lastUpdate  time.Time
	updateCount uint64
	err         error
	conn        *websocket.Conn
}

func initialModel(addr string) model {
	return model{serverAddr: addr}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.connectCmd(),
		tickCmd(),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.conn != nil {
				m.conn.CloseNow()
			}
			return m, tea.Quit
		}

	case wsUpdateMsg:
		m.drones = msg.Drones
		m.connected = true
		m.lastUpdate = time.Now()
		m.updateCount++
		m.err = nil
		return m, m.readCmd()

	case wsErrorMsg:
		m.err = msg.err
		m.connected = false
		if m.conn != nil {
			m.conn.CloseNow()
			m.conn = nil
		}
		return m, tea.Tick(2*time.Second, func(_ time.Time) tea.Msg {
			return reconnectMsg{}
		})

	case reconnectMsg:
		return m, m.connectCmd()

	case tickMsg:
		return m, tickCmd()
	}

	return m, nil
}

func (m *model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		conn, _, err := websocket.Dial(ctx, "ws://"+m.serverAddr+"/ws", nil)
		if err != nil {
			return wsErrorMsg{err}
		}
		m.conn = conn

		// Do first read
		_, data, err := conn.Read(ctx)
		if err != nil {
			return wsErrorMsg{err}
		}
		var msg wsMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return wsErrorMsg{err}
		}
		return wsUpdateMsg(msg)
	}
}

func (m *model) readCmd() tea.Cmd {
	conn := m.conn
	if conn == nil {
		return nil
	}
	return func() tea.Msg {
		ctx := context.Background()
		_, data, err := conn.Read(ctx)
		if err != nil {
			return wsErrorMsg{err}
		}
		var msg wsMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return wsErrorMsg{err}
		}
		return wsUpdateMsg(msg)
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) View() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render(" DRONE TELEMETRY DASHBOARD "))
	b.WriteString("\n\n")

	// Connection status line
	if m.connected {
		b.WriteString(connectedStyle.Render("CONNECTED"))
		b.WriteString(cellStyle.Render(fmt.Sprintf("  %s", m.serverAddr)))
	} else {
		b.WriteString(disconnStyle.Render("DISCONNECTED"))
		if m.err != nil {
			b.WriteString(cellStyle.Render(fmt.Sprintf("  %v", m.err)))
		}
	}
	b.WriteString(cellStyle.Render(fmt.Sprintf("  Updates: %d", m.updateCount)))
	b.WriteString("\n\n")

	// Table header
	headers := []string{"ID", "Status", "Armed", "Mode", "Position", "Alt(m)", "Hdg", "Battery", "Last Seen"}
	var hdr []string
	for i, h := range headers {
		hdr = append(hdr, headerStyle.Render(pad(h, colWidths[i])))
	}
	b.WriteString(strings.Join(hdr, " "))
	b.WriteString("\n")
	b.WriteString(headerStyle.Render(strings.Repeat("-", colSum(colWidths)+len(colWidths)-1)))
	b.WriteString("\n")

	// Sort drones
	drones := make([]droneSummary, len(m.drones))
	copy(drones, m.drones)
	sort.Slice(drones, func(i, j int) bool {
		return drones[i].SystemID < drones[j].SystemID
	})

	if len(drones) == 0 {
		b.WriteString(cellStyle.Render("  Waiting for drones..."))
		b.WriteString("\n")
	}

	for _, d := range drones {
		b.WriteString(renderRow(d))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	elapsed := ""
	if !m.lastUpdate.IsZero() {
		elapsed = fmt.Sprintf("  Last: %s ago", fmtDur(time.Since(m.lastUpdate)))
	}
	b.WriteString(statusBarStyle.Render(fmt.Sprintf("Drones: %d%s  |  q to quit", len(drones), elapsed)))

	return b.String()
}

func renderRow(d droneSummary) string {
	c := make([]string, 9)

	c[0] = cellStyle.Render(pad(fmt.Sprintf("%d", d.SystemID), colWidths[0]))

	if d.Connected {
		c[1] = connectedStyle.Render(pad("online", colWidths[1]))
	} else {
		c[1] = disconnStyle.Render(pad("offline", colWidths[1]))
	}

	if d.Armed {
		c[2] = armedStyle.Render(pad("ARMED", colWidths[2]))
	} else {
		c[2] = disarmedStyle.Render(pad("--", colWidths[2]))
	}

	mode := d.FlightMode
	if mode == "" {
		mode = "--"
	}
	c[3] = cellStyle.Render(pad(mode, colWidths[3]))

	if d.Lat != nil && d.Lon != nil {
		c[4] = cellStyle.Render(pad(fmt.Sprintf("%.5f, %.5f", *d.Lat, *d.Lon), colWidths[4]))
	} else {
		c[4] = cellStyle.Render(pad("--", colWidths[4]))
	}

	if d.Alt != nil {
		c[5] = cellStyle.Render(pad(fmt.Sprintf("%.1f", *d.Alt), colWidths[5]))
	} else {
		c[5] = cellStyle.Render(pad("--", colWidths[5]))
	}

	if d.Heading != nil {
		c[6] = cellStyle.Render(pad(fmt.Sprintf("%.0f", *d.Heading), colWidths[6]))
	} else {
		c[6] = cellStyle.Render(pad("--", colWidths[6]))
	}

	if d.BatteryPct != nil {
		pct := *d.BatteryPct
		s := fmt.Sprintf("%d%%", pct)
		if d.BatteryV != nil {
			s = fmt.Sprintf("%d%% %.1fV", pct, *d.BatteryV)
		}
		switch {
		case pct > 50:
			c[7] = battGoodStyle.Render(pad(s, colWidths[7]))
		case pct > 20:
			c[7] = battWarnStyle.Render(pad(s, colWidths[7]))
		default:
			c[7] = battCritStyle.Render(pad(s, colWidths[7]))
		}
	} else {
		c[7] = cellStyle.Render(pad("--", colWidths[7]))
	}

	if d.LastSeenMs > 0 {
		c[8] = cellStyle.Render(pad(fmtDur(time.Since(time.UnixMilli(d.LastSeenMs))), colWidths[8]))
	} else {
		c[8] = cellStyle.Render(pad("--", colWidths[8]))
	}

	return strings.Join(c, " ")
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}

func colSum(a []int) int {
	t := 0
	for _, v := range a {
		t += v
	}
	return t
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%.0fm", d.Minutes())
}

func main() {
	addr := flag.String("addr", "localhost:8080", "Server address (host:port)")
	flag.Parse()

	p := tea.NewProgram(
		initialModel(*addr),
		tea.WithAltScreen(),
	)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
