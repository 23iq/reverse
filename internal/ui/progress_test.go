package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSetupProgressMergesStageTransitions(t *testing.T) {
	model := NewSetupProgressModel(nil)

	next, _ := model.Update(progressEventMsg{event: ProgressEvent{
		Stage:   "Docker",
		Status:  ProgressRunning,
		Message: "Installing packages",
		Command: "apt-get install docker-ce",
	}})
	model = next.(SetupProgressModel)

	next, _ = model.Update(progressEventMsg{event: ProgressEvent{
		Stage:  "Docker",
		Status: ProgressDone,
	}})
	model = next.(SetupProgressModel)

	snapshot := model.Snapshot()
	if len(snapshot.Stages) != 1 {
		t.Fatalf("stage count = %d, want 1", len(snapshot.Stages))
	}
	stage := snapshot.Stages[0]
	if stage.Status != ProgressDone {
		t.Fatalf("status = %q, want %q", stage.Status, ProgressDone)
	}
	if stage.Message != "Installing packages" {
		t.Fatalf("message was not retained: %q", stage.Message)
	}
	if stage.Command != "apt-get install docker-ce" {
		t.Fatalf("command was not retained: %q", stage.Command)
	}
}

func TestSetupProgressFailedEventQuitsWithStageError(t *testing.T) {
	model := NewSetupProgressModel(nil)
	next, cmd := model.Update(progressEventMsg{event: ProgressEvent{
		Stage:   "TLS certificate",
		Status:  ProgressFailed,
		Message: "certificate request failed",
	}})
	model = next.(SetupProgressModel)

	if cmd == nil {
		t.Fatal("failed stage did not request program exit")
	}
	var stageErr *SetupProgressError
	if !errors.As(model.Snapshot().Err, &stageErr) {
		t.Fatalf("error = %T, want *SetupProgressError", model.Snapshot().Err)
	}
	if stageErr.Stage != "TLS certificate" {
		t.Fatalf("failed stage = %q", stageErr.Stage)
	}
}

func TestSetupProgressRetainsWarningState(t *testing.T) {
	model := NewSetupProgressModel(nil)
	next, _ := model.Update(progressEventMsg{event: ProgressEvent{
		Stage:   "Certificate",
		Status:  ProgressWarning,
		Message: "renewal timer was not found",
	}})
	model = next.(SetupProgressModel)

	snapshot := model.Snapshot()
	if snapshot.Stages[0].Status != ProgressWarning {
		t.Fatalf("warning status became %q", snapshot.Stages[0].Status)
	}
	if !strings.Contains(model.View(), "!") {
		t.Fatal("warning stage has no warning marker")
	}
}

func TestSetupProgressSuccessfulCloseCompletesRunningStage(t *testing.T) {
	model := NewSetupProgressModel(nil)
	next, _ := model.Update(progressEventMsg{event: ProgressEvent{
		Stage:  "Caddy",
		Status: ProgressRunning,
	}})
	model = next.(SetupProgressModel)
	next, cmd := model.Update(progressEventsClosedMsg{})
	model = next.(SetupProgressModel)

	snapshot := model.Snapshot()
	if !snapshot.Completed {
		t.Fatal("closed event stream did not complete setup")
	}
	if snapshot.Stages[0].Status != ProgressDone {
		t.Fatalf("running stage ended as %q", snapshot.Stages[0].Status)
	}
	if cmd == nil {
		t.Fatal("completed setup did not request program exit")
	}
}

func TestSetupProgressCtrlCCancelsRunnerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := newSetupProgressModel(ctx, nil, nil, cancel)

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = next.(SetupProgressModel)

	if !model.Snapshot().Cancelled {
		t.Fatal("model was not marked cancelled")
	}
	if cmd == nil {
		t.Fatal("Ctrl-C did not request program exit")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Ctrl-C did not cancel runner context")
	}
}

func TestSetupProgressCtrlCWaitsForTransactionalRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	model := newSetupProgressModel(ctx, nil, result, cancel)

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = next.(SetupProgressModel)
	if cmd != nil {
		t.Fatal("transactional setup quit before its runner completed rollback")
	}
	if !model.Snapshot().Cancelled {
		t.Fatal("model was not marked cancelled")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Ctrl-C did not cancel the runner context")
	}

	next, cmd = model.Update(progressEventsClosedMsg{})
	if cmd == nil {
		t.Fatal("closed event stream did not wait for the runner result")
	}
}

func TestSetupProgressViewShowsDryRunCommand(t *testing.T) {
	model := NewSetupProgressModel(nil)
	model.width = 100
	model.logo.Animated = false
	if err := model.applyProgressEvent(ProgressEvent{
		Stage:   "Container",
		Status:  ProgressRunning,
		Message: "Dry run",
		Command: "docker compose up -d",
	}); err != nil {
		t.Fatalf("applyProgressEvent() error = %v", err)
	}

	view := model.View()
	for _, part := range []string{"Container", "Dry run", "$", "docker compose up -d"} {
		if !strings.Contains(view, part) {
			t.Errorf("view does not contain %q", part)
		}
	}
}
