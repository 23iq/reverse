package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	defaultUIWidth  = 80
	defaultUIHeight = 28
	minPanelWidth   = 24
)

func effectiveWidth(width int) int {
	if width <= 0 {
		return defaultUIWidth
	}
	return width
}

func effectiveHeight(height int) int {
	if height <= 0 {
		return defaultUIHeight
	}
	return height
}

// fitPlainText clips one unstyled line to a terminal cell width. It is used
// before applying color so narrow layouts never leak past the right edge.
func fitPlainText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	if lipgloss.Width(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}

	var out strings.Builder
	for _, r := range value {
		candidate := out.String() + string(r)
		if lipgloss.Width(candidate) > width-1 {
			break
		}
		out.WriteRune(r)
	}
	return out.String() + "…"
}

func fitRenderedLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().
		Inline(true).
		MaxWidth(width).
		Render(value)
}

func panelContentWidth(style lipgloss.Style, totalWidth int) int {
	totalWidth = effectiveWidth(totalWidth)
	return max(1, totalWidth-style.GetHorizontalFrameSize())
}

func renderResponsivePanel(style lipgloss.Style, body string, totalWidth int) string {
	totalWidth = effectiveWidth(totalWidth)
	if totalWidth < minPanelWidth {
		return lipgloss.NewStyle().
			Width(totalWidth).
			MaxWidth(totalWidth).
			Render(body)
	}

	styleWidth := totalWidth -
		style.GetHorizontalBorderSize() -
		style.GetHorizontalMargins()
	return style.
		Width(max(1, styleWidth)).
		MaxWidth(totalWidth).
		Render(body)
}

func centerToWidth(value string, width int) string {
	width = effectiveWidth(width)
	return lipgloss.NewStyle().
		Width(width).
		MaxWidth(width).
		Align(lipgloss.Center).
		Render(value)
}

func compactLine(label, value string, width int) string {
	prefix := label + " "
	if lipgloss.Width(prefix) >= width {
		return labelStyle.Render(fitPlainText(label, width))
	}
	available := max(1, width-lipgloss.Width(prefix))
	return labelStyle.Render(prefix) + valueStyle.Render(fitPlainText(value, available))
}
