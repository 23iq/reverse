package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type EventKind uint8

const (
	EventStatus EventKind = iota
	EventTraffic
	EventRequest
	EventError
	EventLog
	EventSnapshot
)

type Event struct {
	Kind EventKind
	Time time.Time

	Online      bool
	URL         string
	LocalTarget string

	BytesIn  int64
	BytesOut int64
	// TrafficCounted means BytesIn and BytesOut were already delivered through
	// EventTraffic. EventRequest still displays them but does not add them again.
	TrafficCounted bool
	Requests       uint64
	Errors         uint64

	RemoteAddr string
	Method     string
	Path       string
	StatusCode int
	Duration   time.Duration
	Message    string
}

type DashboardOptions struct {
	PublicURL   string
	LocalTarget string
	StartTime   time.Time
	MaxLogLines int
}

type DashboardSnapshot struct {
	Online      bool
	PublicURL   string
	LocalTarget string
	Uptime      time.Duration
	BytesIn     int64
	BytesOut    int64
	Requests    uint64
	Errors      uint64
	LogLines    int
}

type eventMsg struct {
	event Event
}

type eventsClosedMsg struct{}
type dashboardTickMsg time.Time

type DashboardModel struct {
	events <-chan Event

	logo        LogoModel
	viewport    viewport.Model
	width       int
	height      int
	ready       bool
	online      bool
	publicURL   string
	localTarget string
	startedAt   time.Time
	now         time.Time
	bytesIn     int64
	bytesOut    int64
	requests    uint64
	errors      uint64
	logs        []string
	maxLogLines int
	followLogs  bool
	closed      bool
}

// Closing events marks the stream offline but leaves the dashboard visible.
func NewDashboard(events <-chan Event, options DashboardOptions) DashboardModel {
	now := time.Now()
	if options.StartTime.IsZero() {
		options.StartTime = now
	}
	if options.MaxLogLines <= 0 {
		options.MaxLogLines = 1000
	}

	return DashboardModel{
		events:      events,
		logo:        NewLogo(),
		viewport:    viewport.New(80, 12),
		width:       80,
		height:      28,
		publicURL:   options.PublicURL,
		localTarget: options.LocalTarget,
		startedAt:   options.StartTime,
		now:         now,
		maxLogLines: options.MaxLogLines,
		followLogs:  true,
	}
}

func RunDashboard(events <-chan Event, options DashboardOptions) error {
	_, err := tea.NewProgram(NewDashboard(events, options), tea.WithAltScreen()).Run()
	return err
}

func (m DashboardModel) Init() tea.Cmd {
	return tea.Batch(m.logo.Tick(), dashboardTick(), waitForEvent(m.events))
}

func dashboardTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return dashboardTickMsg(t)
	})
}

func waitForEvent(events <-chan Event) tea.Cmd {
	if events == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{event: event}
	}
}

func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.logo.Width = msg.Width
		m.resizeViewport()
		m.ready = true
		return m, nil

	case logoTickMsg:
		var cmd tea.Cmd
		m.logo, cmd = m.logo.Update(msg)
		return m, cmd

	case dashboardTickMsg:
		m.now = time.Time(msg)
		return m, dashboardTick()

	case eventMsg:
		m.applyEvent(msg.event)
		m.refreshLogs()
		return m, waitForEvent(m.events)

	case eventsClosedMsg:
		m.closed = true
		m.appendLog(MutedStyle.Render(timestamp(time.Now()) + " event stream closed"))
		m.refreshLogs()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "end":
			m.followLogs = true
			m.viewport.GotoBottom()
			return m, nil
		case "up", "k", "pgup":
			m.followLogs = false
		}
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *DashboardModel) applyEvent(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}

	switch event.Kind {
	case EventStatus:
		m.online = event.Online
		if event.URL != "" {
			m.publicURL = event.URL
		}
		if event.LocalTarget != "" {
			m.localTarget = event.LocalTarget
		}
		state := "offline"
		if event.Online {
			state = "online"
		}
		message := event.Message
		if message == "" {
			message = "tunnel is " + state
		}
		m.appendLog(formatLogLine(event.Time, message))

	case EventTraffic:
		m.bytesIn = addNonNegative(m.bytesIn, event.BytesIn)
		m.bytesOut = addNonNegative(m.bytesOut, event.BytesOut)

	case EventRequest:
		m.requests++
		if event.Requests > 1 {
			m.requests += event.Requests - 1
		}
		if !event.TrafficCounted {
			m.bytesIn = addNonNegative(m.bytesIn, event.BytesIn)
			m.bytesOut = addNonNegative(m.bytesOut, event.BytesOut)
		}
		m.appendLog(FormatAccessLog(event))

	case EventError:
		m.errors++
		if event.Errors > 1 {
			m.errors += event.Errors - 1
		}
		message := event.Message
		if message == "" {
			message = "unknown tunnel error"
		}
		m.appendLog(ErrorStyle.Render(formatLogLine(event.Time, message)))

	case EventLog:
		if event.Message != "" {
			m.appendLog(formatLogLine(event.Time, event.Message))
		}

	case EventSnapshot:
		m.online = event.Online
		m.bytesIn = nonNegative(event.BytesIn)
		m.bytesOut = nonNegative(event.BytesOut)
		m.requests = event.Requests
		m.errors = event.Errors
		if event.URL != "" {
			m.publicURL = event.URL
		}
		if event.LocalTarget != "" {
			m.localTarget = event.LocalTarget
		}
		if event.Message != "" {
			m.appendLog(formatLogLine(event.Time, event.Message))
		}
	}
}

func (m *DashboardModel) appendLog(line string) {
	if line == "" {
		return
	}
	m.logs = append(m.logs, line)
	if overflow := len(m.logs) - m.maxLogLines; overflow > 0 {
		copy(m.logs, m.logs[overflow:])
		m.logs = m.logs[:m.maxLogLines]
	}
}

func (m *DashboardModel) refreshLogs() {
	m.viewport.SetContent(strings.Join(m.logs, "\n"))
	if m.followLogs {
		m.viewport.GotoBottom()
	}
}

func (m *DashboardModel) resizeViewport() {
	width := m.width - 6
	if width < 20 {
		width = 20
	}
	height := m.height - 20
	if height < 5 {
		height = 5
	}
	m.viewport.Width = width
	m.viewport.Height = height
	m.refreshLogs()
}

func (m DashboardModel) Snapshot() DashboardSnapshot {
	uptime := m.now.Sub(m.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	return DashboardSnapshot{
		Online:      m.online,
		PublicURL:   m.publicURL,
		LocalTarget: m.localTarget,
		Uptime:      uptime,
		BytesIn:     m.bytesIn,
		BytesOut:    m.bytesOut,
		Requests:    m.requests,
		Errors:      m.errors,
		LogLines:    len(m.logs),
	}
}

func (m DashboardModel) View() string {
	if !m.ready {
		return "\n  Starting dashboard..."
	}

	logo := lipgloss.NewStyle().
		Width(m.width).
		Align(lipgloss.Center).
		Render(m.logo.View())

	status := ErrorStyle.Render("● OFFLINE")
	if m.online {
		status = SuccessStyle.Render("● ONLINE")
	}
	if m.closed {
		status += MutedStyle.Render("  event stream closed")
	}

	snapshot := m.Snapshot()
	info := strings.Join([]string{
		status,
		labelValue("Public URL", fallback(m.publicURL, "waiting for server")),
		labelValue("Local target", fallback(m.localTarget, "not set")),
		labelValue("Uptime", FormatDuration(snapshot.Uptime)),
	}, "\n")

	metrics := strings.Join([]string{
		metric("Requests", fmt.Sprintf("%d", m.requests)),
		metric("Incoming", FormatBytes(m.bytesIn)),
		metric("Outgoing", FormatBytes(m.bytesOut)),
		metric("Errors", fmt.Sprintf("%d", m.errors)),
	}, "   ")

	infoPanel := panelStyle.Width(max(24, m.width-6)).Render(info + "\n\n" + metrics)
	logTitle := TitleStyle.Render("Access log") +
		MutedStyle.Render("  ↑/↓ scroll  end follow  q quit")
	logPanel := panelStyle.Width(max(24, m.width-6)).
		Height(max(7, m.viewport.Height+1)).
		Render(logTitle + "\n" + m.viewport.View())

	return logo + "\n" + infoPanel + "\n" + logPanel
}

func metric(label, value string) string {
	return MutedStyle.Render(label+" ") + valueStyle.Render(value)
}

func labelValue(label, value string) string {
	return labelStyle.Render(fmt.Sprintf("%-13s", label)) + " " + valueStyle.Render(value)
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) == "" {
		return fallbackValue
	}
	return sanitizeLine(value, 512)
}

func addNonNegative(total, delta int64) int64 {
	if delta <= 0 {
		return total
	}
	return total + delta
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
