package ui

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"strings"
	"sync/atomic"
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

const circuitWideLogo = `╭─·────────────────────────────────────────────────────·─╮
│    ╦═╗ ╔═╗ ╦  ╦ ╔═╗ ╦═╗ ╔═╗ ╔═╗                      │
│    ╠╦╝ ║╣  ╚╗╔╝ ║╣  ╠╦╝ ╚═╗ ║╣        LOCAL ──▶ EDGE  │
│    ╩╚═ ╚═╝  ╚╝  ╚═╝ ╩╚═ ╚═╝ ╚═╝                      │
╰─·────────────── ENCRYPTED TUNNEL FABRIC ─────────────·─╯`

const circuitCompactLogo = `╭─·── REVERSE ──·─╮
│  LOCAL ──▶ EDGE  │
╰─·────────────·───╯`

const portalWideLogo = `        ╭────────────────────────────────────╮
╾━━━━━━━┥            R E V E R S E           ┝━━━━━━━╼
        │       LOCAL  ─── ◈ ───  PUBLIC      │
╾━━━━━━━┥          PRIVATE BY DESIGN          ┝━━━━━━━╼
        ╰────────────────────────────────────╯`

const portalCompactLogo = `╾━━━━━━ REVERSE ━━━━━━╼
    LOCAL ── ◈ ── EDGE
╾━━━━━━━━━━━━━━━━━━━━━╼`

const relayWideLogo = `       ╭──────────────╮          ╭──────────────╮
 ·· ──┤  PRIVATE APP ├──────────┤  PUBLIC EDGE ├── ··
       ╰──────┬───────╯  ◇  ◇  ◇ ╰───────┬──────╯
              ╰──────── R E V E R S E ────╯
                  HTTP + WebSocket`

const relayCompactLogo = `·· PRIVATE ──◇── PUBLIC ··
       R E V E R S E
··──── secure tunnel ────··`

type rgb struct {
	r uint8
	g uint8
	b uint8
}

type logoMotion uint8

const (
	logoWave logoMotion = iota
	logoScanner
	logoPulse
	logoSpark
)

type logoVariant struct {
	name     string
	wide     string
	compact  string
	micro    string
	palette  []rgb
	motion   logoMotion
	interval time.Duration
}

var logoPalette = []rgb{
	{139, 92, 246},
	{99, 102, 241},
	{34, 211, 238},
	{52, 211, 153},
	{34, 211, 238},
	{139, 92, 246},
}

var logoVariants = []logoVariant{
	{
		name:     "aurora",
		wide:     wideLogo,
		compact:  compactLogo,
		micro:    "◈ REVERSE ◈",
		palette:  logoPalette,
		motion:   logoWave,
		interval: 85 * time.Millisecond,
	},
	{
		name:    "circuit",
		wide:    circuitWideLogo,
		compact: circuitCompactLogo,
		micro:   "·─ REVERSE ─·",
		palette: []rgb{
			{34, 211, 238},
			{59, 130, 246},
			{167, 139, 250},
			{244, 114, 182},
			{34, 211, 238},
		},
		motion:   logoScanner,
		interval: 75 * time.Millisecond,
	},
	{
		name:    "portal",
		wide:    portalWideLogo,
		compact: portalCompactLogo,
		micro:   "╾ REVERSE ╼",
		palette: []rgb{
			{16, 185, 129},
			{45, 212, 191},
			{34, 211, 238},
			{14, 165, 233},
			{16, 185, 129},
		},
		motion:   logoPulse,
		interval: 95 * time.Millisecond,
	},
	{
		name:    "relay",
		wide:    relayWideLogo,
		compact: relayCompactLogo,
		micro:   "·· REVERSE ··",
		palette: []rgb{
			{251, 113, 133},
			{244, 114, 182},
			{192, 132, 252},
			{129, 140, 248},
			{251, 113, 133},
		},
		motion:   logoSpark,
		interval: 80 * time.Millisecond,
	},
}

var (
	logoVariantSeed     = randomLogoSeed()
	logoVariantSequence atomic.Uint64
)

type logoTickMsg time.Time

type LogoModel struct {
	Frame    int
	Width    int
	Animated bool
	Compact  bool

	variant int
}

// NewLogo selects a different visual theme on consecutive constructions and
// randomizes the first theme for every process. The animation only changes
// color and same-width decorative glyphs, so every frame occupies the same
// terminal rectangle.
func NewLogo() LogoModel {
	sequence := logoVariantSequence.Add(1) - 1
	variant := int((logoVariantSeed + sequence) % uint64(len(logoVariants)))
	return newLogoWithVariant(variant)
}

func newLogoWithVariant(variant int) LogoModel {
	if variant < 0 || variant >= len(logoVariants) {
		variant = 0
	}
	return LogoModel{
		Width:    80,
		Animated: true,
		variant:  variant,
	}
}

func randomLogoSeed() uint64 {
	var value [8]byte
	if _, err := rand.Read(value[:]); err == nil {
		return binary.LittleEndian.Uint64(value[:])
	}
	return uint64(time.Now().UnixNano())
}

func (m LogoModel) Tick() tea.Cmd {
	if !m.Animated {
		return nil
	}
	variant := m.logoVariant()
	interval := variant.interval
	if interval <= 0 {
		interval = 90 * time.Millisecond
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
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
	variant := m.logoVariant()
	art := logoArtForWidth(variant, m.Width, m.Compact)
	frame := m.Frame
	if !m.Animated {
		frame = 0
	}
	return gradientVariantText(normalizeLogoCanvas(art), frame, variant)
}

func (m LogoModel) logoVariant() logoVariant {
	if m.variant < 0 || m.variant >= len(logoVariants) {
		return logoVariants[0]
	}
	return logoVariants[m.variant]
}

func logoArtForWidth(variant logoVariant, width int, compact bool) string {
	if width <= 0 {
		width = 80
	}

	wide := normalizeLogoCanvas(variant.wide)
	compactArt := normalizeLogoCanvas(variant.compact)
	micro := normalizeLogoCanvas(variant.micro)

	switch {
	case !compact && logoWidth(wide) <= width:
		return wide
	case logoWidth(compactArt) <= width:
		return compactArt
	case logoWidth(micro) <= width:
		return micro
	default:
		return fitPlainText("REVERSE", width)
	}
}

func normalizeLogoCanvas(art string) string {
	art = strings.Trim(art, "\n")
	if art == "" {
		return ""
	}
	lines := strings.Split(art, "\n")
	maxWidth := 0
	for _, line := range lines {
		if width := lipgloss.Width(line); width > maxWidth {
			maxWidth = width
		}
	}
	for index, line := range lines {
		if padding := maxWidth - lipgloss.Width(line); padding > 0 {
			lines[index] += strings.Repeat(" ", padding)
		}
	}
	return strings.Join(lines, "\n")
}

func logoWidth(art string) int {
	width, _ := lipgloss.Size(art)
	return width
}

// gradientText is kept as the default aurora renderer for callers that only
// need a colored text treatment.
func gradientText(text string, frame int) string {
	return gradientVariantText(normalizeLogoCanvas(text), frame, logoVariants[0])
}

func gradientVariantText(text string, frame int, variant logoVariant) string {
	lines := strings.Split(text, "\n")
	maxWidth := max(1, logoWidth(text))

	var out strings.Builder
	for y, line := range lines {
		runes := []rune(line)
		for x, r := range runes {
			r = animatedLogoRune(r, x, y, frame, variant.motion)
			if r == ' ' {
				out.WriteRune(r)
				continue
			}
			position := logoColorPosition(x, y, maxWidth, frame, variant.motion)
			color := paletteColorFrom(variant.palette, position)
			out.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(color)).
				Render(string(r)))
		}
		if y < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func logoColorPosition(x, y, width, frame int, motion logoMotion) float64 {
	normalizedX := float64(x) / float64(max(1, width))
	normalizedY := float64(y) * 0.045
	phase := float64(frame%240) / 60

	switch motion {
	case logoScanner:
		scanner := math.Mod(float64(frame)/18, 1.35) - 0.15
		distance := math.Abs(normalizedX - scanner)
		return normalizedX*0.35 + normalizedY + math.Max(0, 0.32-distance)*2.6
	case logoPulse:
		pulse := (math.Sin(float64(frame)/7+normalizedX*math.Pi*2) + 1) / 2
		return normalizedX*0.45 + normalizedY + pulse*0.42
	case logoSpark:
		spark := float64((x*17+y*31+frame*3)%37) / 37
		return normalizedX*0.55 + normalizedY + spark*0.48
	default:
		return normalizedX + normalizedY + phase/4
	}
}

func animatedLogoRune(r rune, x, y, frame int, motion logoMotion) rune {
	if r != '·' {
		return r
	}
	phase := (x + y*7 + frame) % 12
	switch motion {
	case logoScanner:
		if phase < 2 {
			return '◆'
		}
	case logoSpark:
		if phase == 0 {
			return '✦'
		}
		if phase < 4 {
			return '•'
		}
	default:
		if phase < 3 {
			return '•'
		}
	}
	return '·'
}

func paletteColor(position float64) string {
	return paletteColorFrom(logoPalette, position)
}

func paletteColorFrom(palette []rgb, position float64) string {
	if len(palette) == 0 {
		return "#FFFFFF"
	}
	if len(palette) == 1 {
		return hexColor(palette[0])
	}
	for position < 0 {
		position++
	}
	position = position - math.Floor(position)
	scaled := position * float64(len(palette)-1)
	index := int(scaled)
	if index >= len(palette)-1 {
		index = len(palette) - 2
	}
	fraction := scaled - float64(index)
	start := palette[index]
	end := palette[index+1]

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
