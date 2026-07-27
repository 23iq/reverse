package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	primaryColor = lipgloss.Color("#8B5CF6")
	accentColor  = lipgloss.Color("#22D3EE")
	successColor = lipgloss.Color("#34D399")
	warningColor = lipgloss.Color("#FBBF24")
	errorColor   = lipgloss.Color("#FB7185")
	mutedColor   = lipgloss.Color("#7C8396")
	panelColor   = lipgloss.Color("#34384A")

	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F8FAFC"))

	ErrorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(errorColor)

	SuccessStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(successColor)

	WarningStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(warningColor)

	MutedStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	KeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(accentColor)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(panelColor).
			Padding(1, 2)

	labelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#CBD5E1"))

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F8FAFC"))
)

type HelpEntry struct {
	Command     string
	Description string
}

func RenderError(err error) string {
	if err == nil {
		return ""
	}
	return ErrorStyle.Render("Error: " + err.Error())
}

func RenderHint(text string) string {
	return MutedStyle.Render(text)
}

func RenderHelp(title string, entries []HelpEntry) string {
	if len(entries) == 0 {
		return TitleStyle.Render(title)
	}

	width := 0
	for _, entry := range entries {
		if len(entry.Command) > width {
			width = len(entry.Command)
		}
	}

	var b strings.Builder
	b.WriteString(TitleStyle.Render(title))
	b.WriteString("\n\n")
	for i, entry := range entries {
		command := fmt.Sprintf("%-*s", width, entry.Command)
		b.WriteString("  ")
		b.WriteString(KeyStyle.Render(command))
		b.WriteString("  ")
		b.WriteString(entry.Description)
		if i < len(entries)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
