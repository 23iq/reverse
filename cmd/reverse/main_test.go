package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/23iq/reverse/internal/cli"
	"github.com/23iq/reverse/internal/config"
	"github.com/23iq/reverse/internal/daemon"
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

func TestResolveLocalAddressMigratesLegacyLoopbackToLocalhost(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	got, err := resolveLocalAddress(context.Background(), cli.Options{Port: port}, config.Client{
		LocalHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("resolveLocalAddress() error = %v", err)
	}
	if want := net.JoinHostPort("localhost", fmt.Sprint(port)); got != want {
		t.Fatalf("resolveLocalAddress() = %q, want %q", got, want)
	}
}

func TestResolveLocalAddressHonorsExplicitHostExactly(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	got, err := resolveLocalAddress(context.Background(), cli.Options{
		Port: port,
		Host: "127.0.0.1",
	}, config.Client{LocalHost: "localhost"})
	if err != nil {
		t.Fatalf("resolveLocalAddress() error = %v", err)
	}
	if want := net.JoinHostPort("127.0.0.1", fmt.Sprint(port)); got != want {
		t.Fatalf("resolveLocalAddress() = %q, want %q", got, want)
	}
}

func TestResolveLocalAddressCanReachIPv6Localhost(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	got, err := resolveLocalAddress(context.Background(), cli.Options{Port: port}, config.Client{
		LocalHost: "127.0.0.1",
	})
	if err != nil {
		t.Skipf("localhost does not resolve to IPv6 in this environment: %v", err)
	}
	if want := net.JoinHostPort("localhost", fmt.Sprint(port)); got != want {
		t.Fatalf("resolveLocalAddress() = %q, want %q", got, want)
	}
}

func TestRunDaemonStatusWhenStopped(t *testing.T) {
	t.Setenv("REVERSE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	var output bytes.Buffer
	if err := runDaemonStatus(&output); err != nil {
		t.Fatalf("runDaemonStatus() error = %v", err)
	}
	if got := output.String(); got != "Status: stopped\n" {
		t.Fatalf("status output = %q", got)
	}
}

func TestDaemonEnvironmentExcludesCredentialVariables(t *testing.T) {
	got, err := daemonEnvironment([]string{
		"PATH=/usr/bin",
		"REVERSE_CONFIG=config/config.json",
		"REVERSE_RUNTIME_DIR=/tmp/reverse-runtime",
		"REVERSE_DOMAIN=tunnel.example.com",
		"REVERSE_EMAIL=admin@example.com",
		"REVERSE_PASSWORD=do-not-inherit",
	}, "/workspace")
	if err != nil {
		t.Fatalf("daemonEnvironment() error = %v", err)
	}
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"REVERSE_DOMAIN=", "REVERSE_EMAIL=", "REVERSE_PASSWORD="} {
		if strings.Contains(joined, secret) {
			t.Fatalf("daemon environment contains %s: %q", secret, got)
		}
	}
	for _, required := range []string{"PATH=/usr/bin", "REVERSE_CONFIG=/workspace/config/config.json", "REVERSE_RUNTIME_DIR=/tmp/reverse-runtime"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("daemon environment dropped %s: %q", required, got)
		}
	}
}

func TestRunDaemonStatusDoesNotReportLiveLockedWorkerAsStopped(t *testing.T) {
	t.Setenv("REVERSE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	paths, err := daemon.RuntimePaths()
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := daemon.NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteState(paths, daemon.State{
		InstanceID:  instanceID,
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		PublicURL:   "https://tunnel.example.com",
		LocalTarget: "localhost:3000",
		Status:      "starting",
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := daemon.AcquireLock(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	var output bytes.Buffer
	if err := runDaemonStatus(&output); err != nil {
		t.Fatalf("runDaemonStatus() error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, "Status: running") || !strings.Contains(got, "Control: unavailable") {
		t.Fatalf("status output = %q", got)
	}
}

func TestDaemonTrafficCountersNeverRegress(t *testing.T) {
	newer := time.Now().UTC()
	state := daemon.State{
		BytesIn:     500,
		BytesOut:    800,
		LastEventAt: newer,
	}
	updateDaemonTrafficState(&state, tunnel.Traffic{
		At:             newer.Add(-time.Second),
		TotalToLocal:   400,
		TotalFromLocal: 700,
	})
	if state.BytesIn != 500 || state.BytesOut != 800 || !state.LastEventAt.Equal(newer) {
		t.Fatalf("older traffic regressed state: %#v", state)
	}
	updateDaemonTrafficState(&state, tunnel.Traffic{
		At:             newer.Add(time.Second),
		TotalToLocal:   900,
		TotalFromLocal: 750,
	})
	if state.BytesIn != 900 || state.BytesOut != 800 || !state.LastEventAt.Equal(newer.Add(time.Second)) {
		t.Fatalf("new traffic state = %#v", state)
	}
}

func TestRunDaemonStopCleansStaleState(t *testing.T) {
	t.Setenv("REVERSE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	paths, err := daemon.RuntimePaths()
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := daemon.NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteState(paths, daemon.State{
		InstanceID:  instanceID,
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		PublicURL:   "https://tunnel.example.com",
		LocalTarget: "localhost:3000",
		Status:      string(tunnel.StatusOnline),
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runDaemonStop(context.Background(), &output); err != nil {
		t.Fatalf("runDaemonStop() error = %v", err)
	}
	if !strings.Contains(output.String(), "already stopped") {
		t.Fatalf("stop output = %q", output.String())
	}
	if _, err := daemon.ReadState(paths); !errors.Is(err, daemon.ErrNotRunning) {
		t.Fatalf("stale state remains: %v", err)
	}
}

func TestDaemonWorkerRunsHeadlessAndKeepsSecretsOutOfState(t *testing.T) {
	if !daemon.Supported() {
		t.Skip("daemon mode is unavailable on this platform")
	}
	local, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	localPort := local.Addr().(*net.TCPAddr).Port

	unusedServer, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverPort := unusedServer.Addr().(*net.TCPAddr).Port
	_ = unusedServer.Close()

	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.json")
	runtimePath := filepath.Join(root, "runtime")
	t.Setenv("REVERSE_CONFIG", configPath)
	t.Setenv("REVERSE_RUNTIME_DIR", runtimePath)
	const password = "daemon-test-password-never-in-state"
	if err := config.SaveClient(configPath, config.Client{
		ServerURL: fmt.Sprintf("https://127.0.0.1:%d", serverPort),
		Password:  password,
		LocalHost: "localhost",
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := daemon.RuntimePaths()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runDaemonWorker(ctx, cli.Options{
			Action: cli.ActionDaemonWorker,
			Port:   localPort,
			Host:   "127.0.0.1",
		})
	}()

	var live daemon.State
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		live, err = daemon.Query(paths)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("daemon never became queryable: %v", err)
	}
	if live.LocalTarget != net.JoinHostPort("127.0.0.1", fmt.Sprint(localPort)) {
		cancel()
		t.Fatalf("LocalTarget = %q", live.LocalTarget)
	}
	rawState, err := os.ReadFile(paths.State)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if strings.Contains(string(rawState), password) {
		cancel()
		t.Fatal("daemon state contains the client password")
	}
	if _, err := daemon.Stop(paths); err != nil {
		cancel()
		t.Fatalf("daemon.Stop() error = %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runDaemonWorker() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("daemon worker did not stop")
	}
	if _, err := daemon.ReadState(paths); !errors.Is(err, daemon.ErrNotRunning) {
		t.Fatalf("state remains after clean stop: %v", err)
	}
}
