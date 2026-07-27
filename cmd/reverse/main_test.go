package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/23iq/reverse/internal/cli"
	"github.com/23iq/reverse/internal/setup"
	"github.com/23iq/reverse/internal/tunnel"
	"github.com/23iq/reverse/internal/ui"
)

func TestRunHelp(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := run(context.Background(), cli.Options{Action: cli.ActionHelp}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(output.String(), "reverse - expose a local HTTP service") {
		t.Fatalf("help output is missing heading: %q", output.String())
	}
}

func TestCredentialsFromEnvironmentPreservePassword(t *testing.T) {
	t.Setenv("REVERSE_DOMAIN", "tunnel.example.com")
	t.Setenv("REVERSE_PASSWORD", "  päss word  ")
	domain, password, err := credentials(func() (string, string, error) {
		t.Fatal("interactive form should not be called")
		return "", "", nil
	})
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	if domain != "tunnel.example.com" || password != "  päss word  " {
		t.Fatalf("credentials() = %q, %q", domain, password)
	}
}

func TestFriendlyTunnelErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		text string
	}{
		{tunnel.ErrAuthenticationRejected, "authentication failed"},
		{tunnel.ErrPublicPortInUse, "already occupied on the VPS"},
		{tunnel.ErrTunnelBusy, "already connected"},
	}
	for _, test := range tests {
		got := friendlyTunnelError(test.err, 3000)
		if !strings.Contains(got.Error(), test.text) {
			t.Errorf("friendlyTunnelError(%v) = %q", test.err, got)
		}
	}

	original := errors.New("network unavailable")
	if got := friendlyTunnelError(original, 3000); !errors.Is(got, original) {
		t.Fatalf("unclassified error = %v, want original", got)
	}
}

func TestCanonicalDomain(t *testing.T) {
	t.Parallel()
	if got := canonicalDomain(" Tunnel.Example.COM.:443 "); got != "tunnel.example.com" {
		t.Fatalf("canonicalDomain() = %q", got)
	}
	if got := canonicalDomain("Tunnel.Example.COM."); got != "tunnel.example.com" {
		t.Fatalf("canonicalDomain() = %q", got)
	}
}

func TestProgressStatusMapping(t *testing.T) {
	t.Parallel()
	if got := progressStatus(setup.StatusSuccess); got != ui.ProgressDone {
		t.Fatalf("success maps to %q", got)
	}
	if got := progressStatus(setup.StatusRunning); got != ui.ProgressRunning {
		t.Fatalf("running maps to %q", got)
	}
	if got := progressStatus(setup.StatusWarning); got != ui.ProgressWarning {
		t.Fatalf("warning maps to %q", got)
	}
}

func TestDashboardBridgeCoalescesTraffic(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newDashboardBridge(ctx, "https://tunnel.example.com", "127.0.0.1:3000")
	defer bridge.close()

	bridge.onTunnelTraffic(tunnel.Traffic{Direction: tunnel.TrafficToLocal, Bytes: 100})
	bridge.onTunnelTraffic(tunnel.Traffic{Direction: tunnel.TrafficFromLocal, Bytes: 250})

	select {
	case event := <-bridge.events:
		if event.Kind != ui.EventTraffic || event.BytesIn != 100 || event.BytesOut != 250 {
			t.Fatalf("traffic event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced traffic")
	}
}
