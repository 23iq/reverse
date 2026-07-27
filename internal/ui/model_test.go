package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
