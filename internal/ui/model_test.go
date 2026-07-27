package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestFormTransitionsAndSubmission(t *testing.T) {
	model := NewSetupFormModel()
	model.stage = formInput
	model.logo.Animated = false
	model.inputs[0].SetValue("tunnel.example.com")
	model.inputs[0].Focus()

	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(FormModel)
	if model.focus != 1 {
		t.Fatalf("focus = %d, want password field", model.focus)
	}

	model.inputs[1].SetValue("secret")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(FormModel)
	if !model.Submitted() {
		t.Fatal("form was not submitted")
	}
	if cmd == nil {
		t.Fatal("submission did not return a quit command")
	}

	domain, password, err := model.Values()
	if err != nil {
		t.Fatalf("Values() error = %v", err)
	}
	if domain != "tunnel.example.com" || password != "secret" {
		t.Fatalf("Values() = %q, %q", domain, password)
	}
}

func TestFormRejectsEmptyField(t *testing.T) {
	model := NewClientConfigFormModel()
	model.stage = formInput
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(FormModel)

	if model.errText == "" {
		t.Fatal("empty domain was accepted")
	}
	if model.Submitted() {
		t.Fatal("empty form was submitted")
	}
}

func TestFormViewsDoNotJumpOrOverflow(t *testing.T) {
	for variant := range logoVariants {
		model := NewSetupFormModel()
		model.logo = newLogoWithVariant(variant)
		next, _ := model.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
		model = next.(FormModel)

		wantWidth, wantHeight := lipgloss.Size(model.View())
		for frame := 1; frame < introFrames; frame++ {
			model.logo.Frame = frame
			model.introFrame = frame
			gotWidth, gotHeight := lipgloss.Size(model.View())
			if gotWidth != wantWidth || gotHeight != wantHeight {
				t.Fatalf(
					"variant %q form frame %d is %dx%d, want %dx%d",
					logoVariants[variant].name,
					frame,
					gotWidth,
					gotHeight,
					wantWidth,
					wantHeight,
				)
			}
		}
	}

	model := NewClientConfigFormModel()
	model.stage = formInput
	for _, width := range []int{42, 23, 12, 7, 1} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 18})
		resized := next.(FormModel)
		if got := lipgloss.Width(resized.View()); got > width {
			t.Errorf("form width %d rendered %d cells", width, got)
		}
	}
}

func TestDashboardAppliesEvents(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	model := NewDashboard(nil, DashboardOptions{
		PublicURL:   "https://old.example.com",
		LocalTarget: "http://localhost:3000",
		StartTime:   start,
		MaxLogLines: 2,
	})
	model.now = start.Add(90 * time.Second)

	model.applyEvent(Event{
		Kind:        EventStatus,
		Time:        start,
		Online:      true,
		URL:         "https://tunnel.example.com",
		LocalTarget: "http://localhost:8080",
	})
	model.applyEvent(Event{
		Kind:       EventRequest,
		Time:       start,
		Method:     "GET",
		Path:       "/",
		StatusCode: 200,
		BytesIn:    12,
		BytesOut:   34,
	})
	model.applyEvent(Event{
		Kind:    EventError,
		Time:    start,
		Message: "upstream refused connection",
	})

	snapshot := model.Snapshot()
	if !snapshot.Online {
		t.Fatal("dashboard remained offline")
	}
	if snapshot.PublicURL != "https://tunnel.example.com" {
		t.Fatalf("URL = %q", snapshot.PublicURL)
	}
	if snapshot.LocalTarget != "http://localhost:8080" {
		t.Fatalf("target = %q", snapshot.LocalTarget)
	}
	if snapshot.BytesIn != 12 || snapshot.BytesOut != 34 {
		t.Fatalf("traffic = (%d, %d)", snapshot.BytesIn, snapshot.BytesOut)
	}
	if snapshot.Requests != 1 || snapshot.Errors != 1 {
		t.Fatalf("counters = (%d requests, %d errors)", snapshot.Requests, snapshot.Errors)
	}
	if snapshot.Uptime != 90*time.Second {
		t.Fatalf("uptime = %s", snapshot.Uptime)
	}
	if snapshot.LogLines != 2 {
		t.Fatalf("retained log lines = %d, want 2", snapshot.LogLines)
	}
}

func TestDashboardSnapshotReplacesCounters(t *testing.T) {
	model := NewDashboard(nil, DashboardOptions{})
	model.applyEvent(Event{
		Kind:     EventSnapshot,
		Online:   true,
		BytesIn:  100,
		BytesOut: 200,
		Requests: 3,
		Errors:   4,
	})
	model.applyEvent(Event{Kind: EventTraffic, BytesIn: -1, BytesOut: 10})

	snapshot := model.Snapshot()
	if snapshot.BytesIn != 100 || snapshot.BytesOut != 210 {
		t.Fatalf("traffic = (%d, %d), want (100, 210)", snapshot.BytesIn, snapshot.BytesOut)
	}
	if snapshot.Requests != 3 || snapshot.Errors != 4 {
		t.Fatalf("counters = (%d, %d), want (3, 4)", snapshot.Requests, snapshot.Errors)
	}
}

func TestDashboardRequestDoesNotDoubleCountReportedTraffic(t *testing.T) {
	model := NewDashboard(nil, DashboardOptions{})
	model.applyEvent(Event{
		Kind:     EventTraffic,
		BytesIn:  120,
		BytesOut: 450,
	})
	model.applyEvent(Event{
		Kind:           EventRequest,
		Method:         "GET",
		Path:           "/archive.zip",
		BytesIn:        120,
		BytesOut:       450,
		TrafficCounted: true,
	})

	snapshot := model.Snapshot()
	if snapshot.Requests != 1 {
		t.Fatalf("requests = %d, want 1", snapshot.Requests)
	}
	if snapshot.BytesIn != 120 || snapshot.BytesOut != 450 {
		t.Fatalf("traffic = (%d, %d), want (120, 450)", snapshot.BytesIn, snapshot.BytesOut)
	}
	log := model.logs[len(model.logs)-1]
	for _, amount := range []string{"120 B", "450 B"} {
		if !strings.Contains(log, amount) {
			t.Fatalf("access log %q does not show %q", log, amount)
		}
	}
}

func TestDashboardStatusCardStaysPinnedAboveFixedLogViewport(t *testing.T) {
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	model := NewDashboard(nil, DashboardOptions{
		PublicURL:   "https://tunnel.example.com",
		LocalTarget: "http://localhost:3000",
		StartTime:   start,
	})
	model.now = start.Add(time.Minute)
	model.online = true

	next, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 22})
	model = next.(DashboardModel)
	viewportHeight := model.viewport.Height

	for index := 0; index < 200; index++ {
		model.applyEvent(Event{
			Kind:    EventLog,
			Time:    start.Add(time.Duration(index) * time.Second),
			Message: "first batch " + strings.Repeat("x", index%5),
		})
	}
	model.refreshLogs()
	firstView := model.View()
	firstLogOffset := strings.Index(firstView, "Access log")
	if firstLogOffset < 0 {
		t.Fatal("dashboard has no access-log section")
	}
	firstHeader := firstView[:firstLogOffset]

	for index := 0; index < 400; index++ {
		model.applyEvent(Event{
			Kind:    EventLog,
			Time:    start.Add(time.Duration(index+200) * time.Second),
			Message: fmt.Sprintf("later log %03d", index),
		})
	}
	model.refreshLogs()
	secondView := model.View()
	secondLogOffset := strings.Index(secondView, "Access log")
	if secondLogOffset < 0 {
		t.Fatal("dashboard lost its access-log section")
	}
	secondHeader := secondView[:secondLogOffset]

	if firstHeader != secondHeader {
		t.Fatal("status card moved or changed when log volume grew")
	}
	if model.viewport.Height != viewportHeight {
		t.Fatalf(
			"log viewport height changed from %d to %d",
			viewportHeight,
			model.viewport.Height,
		)
	}
	if !strings.Contains(firstHeader, "Public URL") ||
		!strings.Contains(firstHeader, "Local target") ||
		!strings.Contains(firstHeader, "ONLINE") {
		t.Fatalf("pinned card is missing status fields: %q", firstHeader)
	}
	if got := lipgloss.Height(firstView); got != lipgloss.Height(secondView) {
		t.Fatalf(
			"dashboard grew from %d to %d lines after hundreds of logs",
			got,
			lipgloss.Height(secondView),
		)
	}
	if got := lipgloss.Height(secondView); got != 22 {
		t.Fatalf(
			"dashboard height = %d, want terminal height 22 (status=%d header=%d viewport=%d viewport-view=%d panel=%d log=%d)",
			got,
			lipgloss.Height(model.renderStatusCard()),
			lipgloss.Height(secondHeader),
			model.viewport.Height,
			lipgloss.Height(model.viewport.View()),
			lipgloss.Height(model.renderLogPanel()),
			lipgloss.Height(secondView[secondLogOffset:]),
		)
	}
	if strings.Contains(secondView, "later log 000") {
		t.Fatal("fixed viewport rendered logs above its visible window")
	}
	if !strings.Contains(secondView, "later log 399") {
		t.Fatal("following viewport did not retain the newest log")
	}
}

func TestDashboardResponsiveViewsDoNotOverflow(t *testing.T) {
	model := NewDashboard(nil, DashboardOptions{
		PublicURL:   "https://very-long-subdomain.example.com/a/long/path",
		LocalTarget: "http://localhost:3000/a/long/path",
	})
	model.online = true
	model.applyEvent(Event{
		Kind:    EventLog,
		Message: strings.Repeat("long-log-value-", 20),
	})

	for _, size := range []tea.WindowSizeMsg{
		{Width: 80, Height: 22},
		{Width: 42, Height: 16},
		{Width: 23, Height: 12},
		{Width: 12, Height: 8},
		{Width: 7, Height: 6},
		{Width: 1, Height: 3},
	} {
		next, _ := model.Update(size)
		resized := next.(DashboardModel)
		resized.refreshLogs()
		view := resized.View()
		if got := lipgloss.Width(view); got > size.Width {
			t.Errorf(
				"dashboard %dx%d rendered %d cells wide",
				size.Width,
				size.Height,
				got,
			)
		}
		if resized.viewport.Height > 0 && lipgloss.Height(view) > size.Height {
			t.Errorf(
				"dashboard %dx%d rendered %d lines (status=%d viewport=%d viewport-view=%d panel=%d)",
				size.Width,
				size.Height,
				lipgloss.Height(view),
				lipgloss.Height(resized.renderStatusCard()),
				resized.viewport.Height,
				lipgloss.Height(resized.viewport.View()),
				lipgloss.Height(resized.renderLogPanel()),
			)
		}
	}
}

func TestDashboardHeaderHeightIsStableAsCountersGrow(t *testing.T) {
	model := NewDashboard(nil, DashboardOptions{
		PublicURL:   "https://tunnel.example.com",
		LocalTarget: "http://localhost:3000",
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
	model = next.(DashboardModel)
	wantHeight := lipgloss.Height(model.renderStatusCard())

	model.requests = ^uint64(0)
	model.errors = ^uint64(0)
	model.bytesIn = int64(^uint64(0) >> 1)
	model.bytesOut = int64(^uint64(0) >> 1)
	if got := lipgloss.Height(model.renderStatusCard()); got != wantHeight {
		t.Fatalf("counter growth changed status-card height from %d to %d", wantHeight, got)
	}
}
