package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/23iq/reverse/internal/buildinfo"
)

type ProgressStatus string

const (
	ProgressRunning ProgressStatus = "running"
	ProgressDone    ProgressStatus = "done"
	ProgressWarning ProgressStatus = "warning"
	ProgressFailed  ProgressStatus = "failed"
)

type ProgressEvent struct {
	Stage   string
	Status  ProgressStatus
	Message string
	Command string
}

type ProgressStage struct {
	Stage   string
	Status  ProgressStatus
	Message string
	Command string
}

type SetupProgressSnapshot struct {
	Stages    []ProgressStage
	Completed bool
	Cancelled bool
	Err       error
}

// The runner must honor ctx; RunSetupProgress owns and closes events.
type SetupProgressRunner func(ctx context.Context, events chan<- ProgressEvent) error

type SetupProgressError struct {
	Stage   string
	Message string
}

func (e *SetupProgressError) Error() string {
	switch {
	case e.Stage != "" && e.Message != "":
		return fmt.Sprintf("%s: %s", e.Stage, e.Message)
	case e.Stage != "":
		return e.Stage + " failed"
	case e.Message != "":
		return e.Message
	default:
		return "setup failed"
	}
}

type progressEventMsg struct {
	event ProgressEvent
}

type progressEventsClosedMsg struct{}
type progressResultMsg struct {
	err error
}
type progressContextDoneMsg struct {
	err error
}

type SetupProgressModel struct {
	events <-chan ProgressEvent
	result <-chan error
	cancel context.CancelFunc
	ctx    context.Context

	logo       LogoModel
	spinner    spinner.Model
	stages     []ProgressStage
	stageIndex map[string]int
	width      int
	height     int

	completed bool
	cancelled bool
	finalErr  error
}

// Closing events marks a standalone model complete. RunSetupProgress owns
// cancellation and the event channel when it drives the model.
func NewSetupProgressModel(events <-chan ProgressEvent) SetupProgressModel {
	return newSetupProgressModel(nil, events, nil, nil)
}

func newSetupProgressModel(
	ctx context.Context,
	events <-chan ProgressEvent,
	result <-chan error,
	cancel context.CancelFunc,
) SetupProgressModel {
	progressSpinner := spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(KeyStyle),
	)
	return SetupProgressModel{
		events:     events,
		result:     result,
		cancel:     cancel,
		ctx:        ctx,
		logo:       NewLogo(),
		spinner:    progressSpinner,
		stageIndex: make(map[string]int),
		width:      defaultUIWidth,
		height:     defaultUIHeight,
	}
}

// Ctrl-C cancels the runner context but does not return until rollback ends.
func RunSetupProgress(ctx context.Context, run SetupProgressRunner) error {
	if ctx == nil {
		return errors.New("setup progress requires a context")
	}
	if run == nil {
		return errors.New("setup progress requires a runner")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan ProgressEvent, 64)
	result := make(chan error, 1)
	go func() {
		var runErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("setup runner panicked: %v", recovered)
			}
			close(events)
			result <- runErr
		}()
		runErr = run(runCtx, events)
	}()

	model := newSetupProgressModel(runCtx, events, result, cancel)
	final, programErr := tea.NewProgram(model).Run()
	if programErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return programErr
	}

	state, ok := final.(SetupProgressModel)
	if !ok {
		return errors.New("unexpected setup progress state")
	}
	if state.finalErr != nil {
		return state.finalErr
	}
	if state.cancelled {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return context.Canceled
	}
	if !state.completed {
		return errors.New("setup progress stopped before completion")
	}
	return nil
}

func (m SetupProgressModel) Snapshot() SetupProgressSnapshot {
	stages := make([]ProgressStage, len(m.stages))
	copy(stages, m.stages)
	return SetupProgressSnapshot{
		Stages:    stages,
		Completed: m.completed,
		Cancelled: m.cancelled,
		Err:       m.finalErr,
	}
}

func (m SetupProgressModel) Init() tea.Cmd {
	return tea.Batch(
		m.logo.Tick(),
		func() tea.Msg { return m.spinner.Tick() },
		waitForProgressEvent(m.events),
		waitForProgressContext(m.ctx),
	)
}

func waitForProgressEvent(events <-chan ProgressEvent) tea.Cmd {
	if events == nil {
		return func() tea.Msg { return progressEventsClosedMsg{} }
	}
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return progressEventsClosedMsg{}
		}
		return progressEventMsg{event: event}
	}
}

func waitForProgressResult(result <-chan error) tea.Cmd {
	if result == nil {
		return nil
	}
	return func() tea.Msg {
		return progressResultMsg{err: <-result}
	}
}

func waitForProgressContext(ctx context.Context) tea.Cmd {
	if ctx == nil {
		return nil
	}
	return func() tea.Msg {
		<-ctx.Done()
		return progressContextDoneMsg{err: ctx.Err()}
	}
}

func (m SetupProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = effectiveWidth(msg.Width)
		m.height = effectiveHeight(msg.Height)
		m.logo.Width = m.width
		return m, nil

	case logoTickMsg:
		var cmd tea.Cmd
		m.logo, cmd = m.logo.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case progressEventMsg:
		if err := m.applyProgressEvent(msg.event); err != nil {
			m.finalErr = err
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		return m, waitForProgressEvent(m.events)

	case progressEventsClosedMsg:
		m.events = nil
		if m.result != nil {
			return m, waitForProgressResult(m.result)
		}
		m.finishSuccessfully()
		return m, tea.Quit

	case progressResultMsg:
		if msg.err != nil {
			m.failCurrentStage(msg.err)
			m.finalErr = msg.err
		} else {
			m.finishSuccessfully()
		}
		return m, tea.Quit

	case progressContextDoneMsg:
		if m.completed || m.finalErr != nil {
			return m, nil
		}
		m.cancelled = true
		if m.result == nil {
			return m, tea.Quit
		}
		// The setup runner owns transactional rollback. Keep the program alive
		// until it closes the event stream and reports its result so the caller
		// cannot exit the process halfway through restoration.
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancelled = true
			if m.cancel != nil {
				m.cancel()
			}
			if m.result == nil {
				return m, tea.Quit
			}
			return m, nil
		}
	}

	return m, nil
}

func (m *SetupProgressModel) applyProgressEvent(event ProgressEvent) error {
	stageName := strings.TrimSpace(event.Stage)
	if stageName == "" {
		stageName = "Setup"
	}
	status := normalizeProgressStatus(event.Status)
	message := sanitizeLine(event.Message, 2048)
	command := sanitizeLine(event.Command, 4096)

	index, exists := m.stageIndex[stageName]
	if !exists {
		index = len(m.stages)
		m.stageIndex[stageName] = index
		m.stages = append(m.stages, ProgressStage{
			Stage:  sanitizeLine(stageName, 256),
			Status: status,
		})
	}

	stage := &m.stages[index]
	stage.Status = status
	if message != "" {
		stage.Message = message
	}
	if command != "" {
		stage.Command = command
	}

	if status == ProgressFailed {
		return &SetupProgressError{
			Stage:   stage.Stage,
			Message: stage.Message,
		}
	}
	return nil
}

func normalizeProgressStatus(status ProgressStatus) ProgressStatus {
	switch status {
	case ProgressDone, ProgressWarning, ProgressFailed:
		return status
	default:
		return ProgressRunning
	}
}

func (m *SetupProgressModel) finishSuccessfully() {
	for i := range m.stages {
		if m.stages[i].Status == ProgressRunning {
			m.stages[i].Status = ProgressDone
		}
	}
	m.completed = true
}

func (m *SetupProgressModel) failCurrentStage(err error) {
	message := sanitizeLine(err.Error(), 2048)
	for i := len(m.stages) - 1; i >= 0; i-- {
		if m.stages[i].Status == ProgressRunning {
			m.stages[i].Status = ProgressFailed
			if m.stages[i].Message == "" {
				m.stages[i].Message = message
			}
			return
		}
	}

	stage := ProgressStage{
		Stage:   "Setup",
		Status:  ProgressFailed,
		Message: message,
	}
	m.stageIndex[stage.Stage] = len(m.stages)
	m.stages = append(m.stages, stage)
}

func (m SetupProgressModel) View() string {
	width := effectiveWidth(m.width)
	logoModel := m.logo
	logoModel.Width = width
	if effectiveHeight(m.height) < 16 {
		logoModel.Compact = true
	}
	logo := centerToWidth(logoModel.View(), width)

	bodyWidth := width
	if width >= minPanelWidth {
		bodyWidth = panelContentWidth(panelStyle, width)
	}

	var body strings.Builder
	title := "Installing REVERSE " + buildinfo.Version
	body.WriteString(TitleStyle.Render(fitPlainText(title, bodyWidth)))
	body.WriteByte('\n')
	body.WriteString(MutedStyle.Render(fitPlainText(
		"Preparing this server for secure reverse tunnels.",
		bodyWidth,
	)))
	body.WriteString("\n\n")

	if len(m.stages) == 0 {
		body.WriteString(m.spinner.View())
		if bodyWidth > 2 {
			body.WriteString(" ")
			body.WriteString(valueStyle.Render(fitPlainText(
				"Waiting for the installer...",
				bodyWidth-2,
			)))
		}
	} else {
		for index, stage := range m.stages {
			body.WriteString(m.renderProgressStageWidth(stage, bodyWidth))
			if index < len(m.stages)-1 {
				body.WriteByte('\n')
			}
		}
	}

	body.WriteString("\n\n")
	switch {
	case m.finalErr != nil:
		body.WriteString(ErrorStyle.Render(fitPlainText(
			"Setup failed: "+sanitizeLine(m.finalErr.Error(), 2048),
			bodyWidth,
		)))
	case m.cancelled:
		body.WriteString(WarningStyle.Render(fitPlainText(
			"Cancellation requested. Finishing rollback...",
			bodyWidth,
		)))
	case m.completed:
		body.WriteString(SuccessStyle.Render(fitPlainText(
			"Setup completed successfully.",
			bodyWidth,
		)))
	default:
		hint := "ctrl+c  cancel setup"
		if bodyWidth < lipgloss.Width(hint) {
			hint = "ctrl+c cancel"
		}
		key := fitPlainText("ctrl+c", bodyWidth)
		body.WriteString(KeyStyle.Render(key))
		if bodyWidth > lipgloss.Width(key) {
			body.WriteString(MutedStyle.Render(
				fitPlainText(strings.TrimPrefix(hint, "ctrl+c"), bodyWidth-lipgloss.Width(key)),
			))
		}
	}

	panel := renderResponsivePanel(panelStyle, body.String(), width)
	return logo + "\n" + panel
}

func (m SetupProgressModel) renderProgressStage(stage ProgressStage) string {
	width := effectiveWidth(m.width)
	if width >= minPanelWidth {
		width = panelContentWidth(panelStyle, width)
	}
	return m.renderProgressStageWidth(stage, width)
}

func (m SetupProgressModel) renderProgressStageWidth(stage ProgressStage, width int) string {
	var icon string
	switch stage.Status {
	case ProgressDone:
		icon = SuccessStyle.Render("✓")
	case ProgressWarning:
		icon = WarningStyle.Render("!")
	case ProgressFailed:
		icon = ErrorStyle.Render("×")
	default:
		icon = m.spinner.View()
	}
	if width <= lipgloss.Width(icon)+1 {
		return fitRenderedLine(icon, width)
	}

	var row strings.Builder
	row.WriteString(icon)
	row.WriteString(" ")
	row.WriteString(TitleStyle.Render(fitPlainText(stage.Stage, width-2)))
	if stage.Message != "" && width > 2 {
		row.WriteString("\n  ")
		row.WriteString(MutedStyle.Render(fitPlainText(stage.Message, width-2)))
	}
	if stage.Command != "" && width > 4 {
		row.WriteString("\n  ")
		row.WriteString(KeyStyle.Render("$"))
		row.WriteString(" ")
		row.WriteString(MutedStyle.Render(fitPlainText(stage.Command, width-4)))
	}
	return row.String()
}
