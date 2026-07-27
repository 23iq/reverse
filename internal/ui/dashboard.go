package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/23iq/reverse/internal/buildinfo"
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

var (
	dashboardStatusStyle = panelStyle.Copy().Padding(0, 1)
	dashboardLogStyle    = panelStyle.Copy().Padding(0, 1)
)

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

	model := DashboardModel{
		events:      events,
		logo:        NewLogo(),
		viewport:    viewport.New(80, 12),
		width:       defaultUIWidth,
		height:      defaultUIHeight,
		ready:       true,
		publicURL:   options.PublicURL,
		localTarget: options.LocalTarget,
		startedAt:   options.StartTime,
		now:         now,
		maxLogLines: options.MaxLogLines,
		followLogs:  true,
	}
	model.resizeViewport()
	return model
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
		m.width = effectiveWidth(msg.Width)
		m.height = effectiveHeight(msg.Height)
		m.logo.Width = m.width
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
	width := effectiveWidth(m.width)
	height := effectiveHeight(m.height)
	headerHeight := lipgloss.Height(m.renderStatusCard())

	logChromeHeight := 1
	if width >= minPanelWidth {
		logChromeHeight += dashboardLogStyle.GetVerticalFrameSize()
	}
	viewportHeight := height - headerHeight - logChromeHeight
	if viewportHeight < 0 {
		viewportHeight = 0
	}

	viewportWidth := width
	if width >= minPanelWidth {
		viewportWidth = panelContentWidth(dashboardLogStyle, width)
	}
	m.viewport.Width = max(1, viewportWidth)
	m.viewport.Height = viewportHeight
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
		return m.renderStatusCard()
	}

	statusCard := m.renderStatusCard()
	if m.viewport.Height <= 0 {
		return statusCard
	}

	return statusCard + "\n" + m.renderLogPanel()
}

func (m DashboardModel) renderLogPanel() string {
	logTitleWidth := m.viewport.Width
	logTitle := TitleStyle.Render(fitPlainText("Access log", logTitleWidth))
	help := "  ↑/↓ scroll  end follow  q quit"
	if lipgloss.Width("Access log"+help) <= logTitleWidth {
		logTitle += MutedStyle.Render(help)
	}
	logBody := logTitle + "\n" + m.viewport.View()
	return renderResponsivePanel(dashboardLogStyle, logBody, m.width)
}

func (m DashboardModel) renderStatusCard() string {
	width := effectiveWidth(m.width)
	height := effectiveHeight(m.height)
	if height < 12 {
		return m.renderCondensedStatusCard(width, height < 8)
	}

	snapshot := m.Snapshot()
	contentWidth := width
	if width >= minPanelWidth {
		contentWidth = panelContentWidth(dashboardStatusStyle, width)
	}

	metrics := m.renderDashboardMetrics(contentWidth)
	var body string
	if contentWidth >= 54 {
		logo := m.logo
		logo.Compact = true
		logo.Width = min(28, contentWidth/2)
		renderedLogo := logo.View()
		logoWidth := lipgloss.Width(renderedLogo)
		infoWidth := max(20, contentWidth-logoWidth-3)
		info := m.renderDashboardInfo(infoWidth, snapshot)
		top := lipgloss.JoinHorizontal(
			lipgloss.Center,
			renderedLogo,
			strings.Repeat(" ", 3),
			info,
		)
		body = top + "\n" + metrics
	} else {
		body = m.renderDashboardInfo(contentWidth, snapshot) + "\n" + metrics
	}

	return renderResponsivePanel(dashboardStatusStyle, body, width)
}

func (m DashboardModel) renderCondensedStatusCard(width int, plain bool) string {
	contentWidth := width
	style := dashboardStatusStyle
	if !plain && width >= minPanelWidth {
		contentWidth = panelContentWidth(style, width)
	} else {
		plain = true
	}

	body := strings.Join([]string{
		m.dashboardStatusLine(contentWidth, true),
		compactLine("URL", fallback(m.publicURL, "waiting"), contentWidth),
		compactLine("Local", fallback(m.localTarget, "not set"), contentWidth),
	}, "\n")
	if plain {
		return lipgloss.NewStyle().
			Width(width).
			MaxWidth(width).
			Render(body)
	}
	return renderResponsivePanel(style, body, width)
}

func (m DashboardModel) renderDashboardInfo(width int, snapshot DashboardSnapshot) string {
	return strings.Join([]string{
		m.dashboardStatusLine(width, false),
		dashboardLabelValue("Public URL", fallback(m.publicURL, "waiting for server"), width),
		dashboardLabelValue("Local target", fallback(m.localTarget, "not set"), width),
		dashboardLabelValue("Uptime", FormatDuration(snapshot.Uptime), width),
	}, "\n")
}

func (m DashboardModel) dashboardStatusLine(width int, includeName bool) string {
	state := "● OFFLINE"
	statusStyle := ErrorStyle
	if m.online {
		state = "● ONLINE"
		statusStyle = SuccessStyle
	}

	if includeName {
		nameAndBadge := "REVERSE " + buildinfo.Version + "  "
		if lipgloss.Width(nameAndBadge+state) <= width {
			line := TitleStyle.Render("REVERSE") +
				MutedStyle.Render(" "+buildinfo.Version+"  ") +
				statusStyle.Render(state)
			if m.closed && lipgloss.Width(nameAndBadge+state+"  stream closed") <= width {
				line += MutedStyle.Render("  stream closed")
			}
			return line
		}
		return statusStyle.Render(fitPlainText(state, width))
	}

	line := statusStyle.Render(state)
	badge := "  " + buildinfo.Version
	if lipgloss.Width(state+badge) <= width {
		line += MutedStyle.Render(badge)
	}
	if m.closed && lipgloss.Width(state+badge+"  stream closed") <= width {
		line += MutedStyle.Render("  stream closed")
	}
	return line
}

func (m DashboardModel) renderDashboardMetrics(width int) string {
	entries := []string{
		metric("Requests", fmt.Sprintf("%d", m.requests)),
		metric("Incoming", FormatBytes(m.bytesIn)),
		metric("Outgoing", FormatBytes(m.bytesOut)),
		metric("Errors", fmt.Sprintf("%d", m.errors)),
	}
	if width >= 60 {
		return fitRenderedLine(strings.Join(entries, "   "), width)
	}
	return strings.Join([]string{
		fitRenderedLine(entries[0]+"   "+entries[1], width),
		fitRenderedLine(entries[2]+"   "+entries[3], width),
	}, "\n")
}

func metric(label, value string) string {
	return MutedStyle.Render(label+" ") + valueStyle.Render(value)
}

func labelValue(label, value string) string {
	return labelStyle.Render(fmt.Sprintf("%-13s", label)) + " " + valueStyle.Render(value)
}

func dashboardLabelValue(label, value string, width int) string {
	const labelWidth = 13
	if width <= labelWidth+1 {
		return compactLine(label, value, width)
	}
	valueWidth := width - labelWidth - 1
	return labelStyle.Render(fmt.Sprintf("%-13s", fitPlainText(label, labelWidth))) +
		" " +
		valueStyle.Render(fitPlainText(value, valueWidth))
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
