package main

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/23iq/reverse/internal/config"
	"github.com/23iq/reverse/internal/tunnel"
)

// TestDetachedBinaryEndToEnd builds and launches the real executable, lets the
// parent command exit, then exercises status, tunneled HTTP, and graceful stop.
// It is gated because it starts subprocesses and loopback listeners.
func TestDetachedBinaryEndToEnd(t *testing.T) {
	if os.Getenv("REVERSE_RUN_DAEMON_INTEGRATION") != "1" {
		t.Skip("set REVERSE_RUN_DAEMON_INTEGRATION=1 to run detached-binary integration")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the detached-binary integration currently inspects Linux /proc")
	}

	testRoot := t.TempDir()
	binaryPath := filepath.Join(testRoot, "reverse")
	build := exec.Command("go", "build", "-trimpath", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build reverse binary: %v\n%s", err, output)
	}

	localListener, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		t.Skipf("secondary loopback address is unavailable: %v", err)
	}
	localApplication := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Reverse-Integration", "daemon")
		_, _ = fmt.Fprintf(writer, "local:%s", request.URL.RequestURI())
	}))
	localApplication.Listener = localListener
	localApplication.Start()
	defer localApplication.Close()
	localPort := localListener.Addr().(*net.TCPAddr).Port

	const tunnelPassword = "detached-integration-password"
	tunnelServer, err := tunnel.NewServer(tunnel.ServerConfig{
		PublicBindHost: "127.0.0.1",
		Verify: func(_ context.Context, request tunnel.VerifyRequest) error {
			if request.Token != tunnelPassword {
				return tunnel.ErrAuthenticationRejected
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tunnelServer.Close(context.Background())

	tunnelEndpoint := httptest.NewTLSServer(tunnelServer.TunnelHandler())
	defer tunnelEndpoint.Close()
	certificatePath := filepath.Join(testRoot, "tunnel-ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: tunnelEndpoint.Certificate().Raw,
	})
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(testRoot, "config", "client.json")
	runtimePath := filepath.Join(testRoot, "runtime")
	if err := config.SaveClient(configPath, config.Client{
		ServerURL: tunnelEndpoint.URL,
		Password:  tunnelPassword,
		LocalHost: config.DefaultLocalHost,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVERSE_CONFIG", configPath)
	t.Setenv("REVERSE_RUNTIME_DIR", runtimePath)
	t.Setenv("SSL_CERT_FILE", certificatePath)
	t.Setenv("REVERSE_PASSWORD", "must-not-reach-worker-environment")

	stopCommand := func() {
		command := exec.Command(binaryPath, "--stop")
		command.Env = os.Environ()
		_, _ = command.CombinedOutput()
	}
	t.Cleanup(stopCommand)

	start := exec.Command(
		binaryPath,
		"--port", strconv.Itoa(localPort),
		"--host", "127.0.0.2",
		"--background",
	)
	start.Env = os.Environ()
	startOutput, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("start detached tunnel: %v\n%s", err, startOutput)
	}
	if !strings.Contains(string(startOutput), "Background tunnel started") {
		t.Fatalf("unexpected start output:\n%s", startOutput)
	}

	status := exec.Command(binaryPath, "--status")
	status.Env = os.Environ()
	statusOutput, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("query detached tunnel: %v\n%s", err, statusOutput)
	}
	if !strings.Contains(string(statusOutput), "Status: running") {
		t.Fatalf("unexpected running status:\n%s", statusOutput)
	}
	workerPID, err := daemonPID(string(statusOutput))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(workerPID), "environ"))
	if err != nil {
		t.Fatalf("read detached worker environment: %v", err)
	}
	if strings.Contains(string(environment), "REVERSE_PASSWORD=") {
		t.Fatal("detached worker inherited REVERSE_PASSWORD")
	}

	publicURL := "http://127.0.0.1:" + strconv.Itoa(localPort) + "/database?id=42"
	httpClient := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(8 * time.Second)
	var responseBody string
	for time.Now().Before(deadline) {
		response, requestErr := httpClient.Get(publicURL)
		if requestErr == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				responseBody = string(body)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if responseBody != "local:/database?id=42" {
		t.Fatalf("tunneled response = %q, want local application response", responseBody)
	}

	stop := exec.Command(binaryPath, "--stop")
	stop.Env = os.Environ()
	stopOutput, err := stop.CombinedOutput()
	if err != nil {
		t.Fatalf("stop detached tunnel: %v\n%s", err, stopOutput)
	}
	if !strings.Contains(string(stopOutput), "Background tunnel stopped") {
		t.Fatalf("unexpected stop output:\n%s", stopOutput)
	}

	stoppedStatus := exec.Command(binaryPath, "--status")
	stoppedStatus.Env = os.Environ()
	stoppedOutput, err := stoppedStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("query stopped tunnel: %v\n%s", err, stoppedOutput)
	}
	if strings.TrimSpace(string(stoppedOutput)) != "Status: stopped" {
		t.Fatalf("unexpected stopped status:\n%s", stoppedOutput)
	}
}

func daemonPID(status string) (int, error) {
	for _, line := range strings.Split(status, "\n") {
		if value, found := strings.CutPrefix(line, "PID: "); found {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil && pid > 0 {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("daemon status has no valid PID: %q", status)
}
