package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func FormatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	const unit = int64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	divisor := int64(unit)
	unitIndex := 0
	for value := bytes / unit; value >= unit && unitIndex < 4; value /= unit {
		divisor *= unit
		unitIndex++
	}
	units := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(bytes) / float64(divisor)
	return fmt.Sprintf("%.1f %s", value, units[unitIndex])
}

func FormatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	duration = duration.Truncate(time.Second)

	days := duration / (24 * time.Hour)
	duration %= 24 * time.Hour
	hours := duration / time.Hour
	duration %= time.Hour
	minutes := duration / time.Minute
	seconds := (duration % time.Minute) / time.Second

	if days > 0 {
		return fmt.Sprintf("%dd %02dh %02dm %02ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%02dh %02dm %02ds", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02dm %02ds", minutes, seconds)
}

func FormatAccessLog(event Event) string {
	at := event.Time
	if at.IsZero() {
		at = time.Now()
	}

	method := strings.ToUpper(strings.TrimSpace(sanitizeLine(event.Method, 16)))
	if method == "" {
		method = "HTTP"
	}
	path := sanitizeLine(event.Path, 2048)
	if path == "" {
		path = "/"
	}
	remote := sanitizeLine(event.RemoteAddr, 128)
	if remote == "" {
		remote = "-"
	}

	status := "-"
	if event.StatusCode > 0 {
		status = fmt.Sprintf("%d", event.StatusCode)
	}
	statusStyle := MutedStyle
	switch {
	case event.StatusCode >= 500:
		statusStyle = ErrorStyle
	case event.StatusCode >= 400:
		statusStyle = lipgloss.NewStyle().Foreground(warningColor)
	case event.StatusCode >= 200 && event.StatusCode < 400:
		statusStyle = SuccessStyle
	}

	duration := ""
	if event.Duration > 0 {
		duration = "  " + event.Duration.Truncate(time.Millisecond).String()
	}
	traffic := ""
	if event.BytesIn > 0 || event.BytesOut > 0 {
		traffic = fmt.Sprintf("  ↓ %s  ↑ %s", FormatBytes(event.BytesIn), FormatBytes(event.BytesOut))
	}

	return MutedStyle.Render(timestamp(at)+" "+remote+" ") +
		KeyStyle.Render(fmt.Sprintf("%-7s", method)) + " " +
		valueStyle.Render(path) + "  " +
		statusStyle.Render(status) +
		MutedStyle.Render(duration+traffic)
}

func formatLogLine(at time.Time, message string) string {
	return MutedStyle.Render(timestamp(at)+" ") + valueStyle.Render(sanitizeLine(message, 2048))
}

func timestamp(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return at.Format("15:04:05")
}

// sanitizeLine prevents untrusted request data from injecting terminal control
// sequences or breaking the one-event-per-line layout.
func sanitizeLine(value string, limit int) string {
	if value == "" {
		return ""
	}

	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		runes = append(runes[:limit-1], '…')
	}

	var b strings.Builder
	b.Grow(len(value))
	for _, r := range runes {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
