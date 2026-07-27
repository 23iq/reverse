package ui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/23iq/reverse/internal/buildinfo"
)

var ErrCancelled = errors.New("configuration cancelled")

type formKind uint8

const (
	setupForm formKind = iota
	clientConfigForm
)

type formStage uint8

const (
	formIntro formStage = iota
	formInput
	formDone
)

const introFrames = 18

type FormModel struct {
	kind       formKind
	stage      formStage
	logo       LogoModel
	inputs     [2]textinput.Model
	focus      int
	width      int
	height     int
	introFrame int
	errText    string
	submitted  bool
	cancelled  bool
}

func NewSetupFormModel() FormModel {
	return newFormModel(setupForm)
}

func NewClientConfigFormModel() FormModel {
	return newFormModel(clientConfigForm)
}

func newFormModel(kind formKind) FormModel {
	domain := textinput.New()
	domain.Placeholder = "tunnel.example.com"
	domain.Prompt = "› "
	domain.PromptStyle = KeyStyle
	domain.TextStyle = valueStyle
	domain.CharLimit = 253
	domain.Width = 42

	password := textinput.New()
	password.Placeholder = "Authentication password"
	password.Prompt = "› "
	password.PromptStyle = KeyStyle
	password.TextStyle = valueStyle
	password.CharLimit = 256
	password.Width = 42
	password.EchoMode = textinput.EchoPassword
	password.EchoCharacter = '•'

	return FormModel{
		kind:   kind,
		stage:  formIntro,
		logo:   NewLogo(),
		inputs: [2]textinput.Model{domain, password},
		width:  80,
	}
}

func SetupForm() (domain string, password string, err error) {
	return runForm(NewSetupFormModel())
}

func ClientConfigForm() (domain string, password string, err error) {
	return runForm(NewClientConfigFormModel())
}

func runForm(model FormModel) (domain string, password string, err error) {
	final, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
	if err != nil {
		return "", "", err
	}

	result, ok := final.(FormModel)
	if !ok {
		return "", "", errors.New("unexpected form state")
	}
	if result.cancelled || !result.submitted {
		return "", "", ErrCancelled
	}
	return result.Values()
}

// Values returns the trimmed domain and the password exactly as entered.
func (m FormModel) Values() (domain string, password string, err error) {
	domain = strings.TrimSpace(m.inputs[0].Value())
	password = m.inputs[1].Value()
	if domain == "" {
		return "", "", errors.New("domain is required")
	}
	if password == "" {
		return "", "", errors.New("password is required")
	}
	return domain, password, nil
}

func (m FormModel) Submitted() bool {
	return m.submitted
}

func (m FormModel) Cancelled() bool {
	return m.cancelled
}

func (m FormModel) Init() tea.Cmd {
	return m.logo.Tick()
}

func (m FormModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = effectiveWidth(msg.Width)
		m.height = effectiveHeight(msg.Height)
		m.logo.Width = m.width

		panelWidth := min(62, m.width)
		inputWidth := panelWidth
		if panelWidth >= minPanelWidth {
			inputWidth = panelContentWidth(panelStyle, panelWidth)
		}
		inputWidth -= 2
		if inputWidth > 52 {
			inputWidth = 52
		}
		if inputWidth < 1 {
			inputWidth = 1
		}
		m.inputs[0].Width = inputWidth
		m.inputs[1].Width = inputWidth
		return m, nil

	case logoTickMsg:
		var cmd tea.Cmd
		m.logo, cmd = m.logo.Update(msg)
		if m.stage == formIntro {
			m.introFrame++
			if m.introFrame >= introFrames {
				m.stage = formInput
				m.logo.Animated = false
				focusCmd := m.inputs[0].Focus()
				return m, tea.Batch(cmd, focusCmd)
			}
		}
		return m, cmd

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		}
		if m.stage != formInput {
			return m, nil
		}

		switch msg.String() {
		case "tab", "down":
			return m.moveFocus(1)
		case "shift+tab", "up":
			return m.moveFocus(-1)
		case "enter":
			return m.submitOrAdvance()
		}
	}

	if m.stage != formInput {
		return m, nil
	}

	var cmd tea.Cmd
	m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
	m.errText = ""
	return m, cmd
}

func (m FormModel) moveFocus(direction int) (tea.Model, tea.Cmd) {
	m.inputs[m.focus].Blur()
	m.focus = (m.focus + direction + len(m.inputs)) % len(m.inputs)
	m.errText = ""
	return m, m.inputs[m.focus].Focus()
}

func (m FormModel) submitOrAdvance() (tea.Model, tea.Cmd) {
	current := m.inputs[m.focus].Value()
	empty := current == ""
	if m.focus == 0 {
		empty = strings.TrimSpace(current) == ""
	}
	if empty {
		if m.focus == 0 {
			m.errText = "Enter the domain pointing to this server."
		} else {
			m.errText = "Enter the authentication password."
		}
		return m, nil
	}

	if m.focus == 0 {
		return m.moveFocus(1)
	}

	if _, _, err := m.Values(); err != nil {
		m.errText = err.Error()
		return m, nil
	}
	m.stage = formDone
	m.submitted = true
	return m, tea.Quit
}

func (m FormModel) View() string {
	width := effectiveWidth(m.width)
	logoModel := m.logo
	logoModel.Width = width
	if effectiveHeight(m.height) < 16 {
		logoModel.Compact = true
	}
	logo := centerToWidth(logoModel.View(), width)

	if m.stage == formIntro {
		dots := strings.Repeat("·", m.introFrame%4)
		messageText := fmt.Sprintf("Preparing REVERSE %s%-3s", buildinfo.Version, dots)
		message := centerToWidth(
			MutedStyle.Render(fitPlainText(messageText, width)),
			width,
		)
		return logo + "\n" + message
	}

	title := "Server setup"
	description := "Connect a domain and protect the tunnel server."
	action := "Start installation"
	if m.kind == clientConfigForm {
		title = "Client configuration"
		description = "Connect this machine to your REVERSE server."
		action = "Save configuration"
	}

	panelWidth := min(62, width)
	bodyWidth := panelWidth
	if panelWidth >= minPanelWidth {
		bodyWidth = panelContentWidth(panelStyle, panelWidth)
	}

	fields := strings.Join([]string{
		TitleStyle.Render(fitPlainText(title, bodyWidth)),
		MutedStyle.Render(fitPlainText(description, bodyWidth)),
		"",
		labelStyle.Render(fitPlainText("Domain", bodyWidth)),
		fitRenderedLine(m.inputs[0].View(), bodyWidth),
		"",
		labelStyle.Render(fitPlainText("Password", bodyWidth)),
		fitRenderedLine(m.inputs[1].View(), bodyWidth),
	}, "\n")

	if m.errText != "" {
		fields += "\n\n" + ErrorStyle.Render(fitPlainText(m.errText, bodyWidth))
	}
	if bodyWidth >= 48 {
		fields += "\n\n" + KeyStyle.Render("enter") + MutedStyle.Render("  "+action)
		fields += "   " + KeyStyle.Render("tab") + MutedStyle.Render("  next field")
		fields += "   " + KeyStyle.Render("esc") + MutedStyle.Render("  cancel")
	} else {
		actionHint := KeyStyle.Render("enter") + MutedStyle.Render(
			fitPlainText("  "+action, max(1, bodyWidth-lipgloss.Width("enter"))),
		)
		navigationHint := KeyStyle.Render("tab") + MutedStyle.Render("  next") +
			"   " + KeyStyle.Render("esc") + MutedStyle.Render("  cancel")
		fields += "\n\n" + fitRenderedLine(actionHint, bodyWidth)
		fields += "\n" + fitRenderedLine(navigationHint, bodyWidth)
	}

	panel := renderResponsivePanel(panelStyle, fields, panelWidth)
	panel = centerToWidth(panel, width)
	return logo + "\n" + panel
}
