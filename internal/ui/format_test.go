package ui

import (
	"strings"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		bytes int64
		want  string
	}{
		{-1, "0 B"},
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	}

	for _, test := range tests {
		if got := FormatBytes(test.bytes); got != test.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", test.bytes, got, test.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		duration time.Duration
		want     string
	}{
		{-time.Second, "00m 00s"},
		{0, "00m 00s"},
		{5*time.Minute + 7*time.Second, "05m 07s"},
		{2*time.Hour + 3*time.Minute + 4*time.Second, "02h 03m 04s"},
		{25*time.Hour + 2*time.Minute + 9*time.Second, "1d 01h 02m 09s"},
	}

	for _, test := range tests {
		if got := FormatDuration(test.duration); got != test.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", test.duration, got, test.want)
		}
	}
}

func TestFormatAccessLogContainsRequestDetails(t *testing.T) {
	t.Parallel()

	line := FormatAccessLog(Event{
		Time:       time.Date(2025, 1, 1, 12, 34, 56, 0, time.UTC),
		RemoteAddr: "192.0.2.10",
		Method:     "get",
		Path:       "/download",
		StatusCode: 200,
		BytesOut:   1536,
	})

	for _, part := range []string{"12:34:56", "192.0.2.10", "GET", "/download", "200", "1.5 KiB"} {
		if !strings.Contains(line, part) {
			t.Errorf("access log %q does not contain %q", line, part)
		}
	}
}

func TestSanitizeLineStripsTerminalControls(t *testing.T) {
	t.Parallel()

	line := sanitizeLine("GET\x1b[31m /first\nforged\rentry", 128)
	if strings.ContainsRune(line, '\x1b') {
		t.Fatalf("sanitized line retained escape byte: %q", line)
	}
	if strings.ContainsRune(line, '\n') || strings.ContainsRune(line, '\r') {
		t.Fatalf("sanitized value broke onto another line: %q", line)
	}
}
