package ui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const wideLogo = `██████╗ ███████╗██╗   ██╗███████╗██████╗ ███████╗███████╗
██╔══██╗██╔════╝██║   ██║██╔════╝██╔══██╗██╔════╝██╔════╝
██████╔╝█████╗  ██║   ██║█████╗  ██████╔╝███████╗█████╗
██╔══██╗██╔══╝  ╚██╗ ██╔╝██╔══╝  ██╔══██╗╚════██║██╔══╝
██║  ██║███████╗ ╚████╔╝ ███████╗██║  ██║███████║███████╗
╚═╝  ╚═╝╚══════╝  ╚═══╝  ╚══════╝╚═╝  ╚═╝╚══════╝╚══════╝`

const compactLogo = `╦═╗╔═╗╦  ╦╔═╗╦═╗╔═╗╔═╗
╠╦╝║╣ ╚╗╔╝║╣ ╠╦╝╚═╗║╣
╩╚═╚═╝ ╚╝ ╚═╝╩╚═╚═╝╚═╝`

var logoPalette = []rgb{
	{139, 92, 246},
	{99, 102, 241},
	{34, 211, 238},
	{52, 211, 153},
	{34, 211, 238},
	{139, 92, 246},
}

type rgb struct {
	r uint8
	g uint8
	b uint8
}

type logoTickMsg time.Time

type LogoModel struct {
	Frame    int
	Width    int
	Animated bool
	Compact  bool
}

func NewLogo() LogoModel {
	return LogoModel{
		Width:    80,
		Animated: true,
	}
}

func (m LogoModel) Tick() tea.Cmd {
	if !m.Animated {
		return nil
	}
	return tea.Tick(90*time.Millisecond, func(t time.Time) tea.Msg {
		return logoTickMsg(t)
	})
}

func (m LogoModel) Update(msg tea.Msg) (LogoModel, tea.Cmd) {
	if _, ok := msg.(logoTickMsg); !ok || !m.Animated {
		return m, nil
	}
	m.Frame++
	return m, m.Tick()
}

func (m LogoModel) View() string {
	art := wideLogo
	if m.Compact || (m.Width > 0 && m.Width < 68) {
		art = compactLogo
	}

	frame := m.Frame
	if !m.Animated {
		frame = 0
	}

	rendered := gradientText(art, frame)
	if m.Animated {
		bounce := []int{1, 1, 0, 0, 0, 1, 2, 1}[frame%8]
		rendered = strings.Repeat("\n", bounce) + rendered
	}
	return rendered
}

func gradientText(text string, frame int) string {
	lines := strings.Split(text, "\n")
	maxWidth := 1
	for _, line := range lines {
		if width := len([]rune(line)); width > maxWidth {
			maxWidth = width
		}
	}

	var out strings.Builder
	for y, line := range lines {
		runes := []rune(line)
		for x, r := range runes {
			if r == ' ' {
				out.WriteRune(r)
				continue
			}
			position := float64(x)/float64(maxWidth) + float64(frame%60)/60 + float64(y)*0.025
			color := paletteColor(position)
			out.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(string(r)))
		}
		if y < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func paletteColor(position float64) string {
	for position < 0 {
		position++
	}
	position = position - float64(int(position))
	scaled := position * float64(len(logoPalette)-1)
	index := int(scaled)
	if index >= len(logoPalette)-1 {
		index = len(logoPalette) - 2
	}
	fraction := scaled - float64(index)
	start := logoPalette[index]
	end := logoPalette[index+1]

	mix := func(a, b uint8) uint8 {
		return uint8(float64(a) + (float64(b)-float64(a))*fraction)
	}
	color := rgb{mix(start.r, end.r), mix(start.g, end.g), mix(start.b, end.b)}
	return hexColor(color)
}

func hexColor(color rgb) string {
	const digits = "0123456789ABCDEF"
	bytes := []byte{'#', 0, 0, 0, 0, 0, 0}
	values := []uint8{color.r, color.g, color.b}
	for i, value := range values {
		bytes[1+i*2] = digits[value>>4]
		bytes[2+i*2] = digits[value&0x0f]
	}
	return string(bytes)
}
