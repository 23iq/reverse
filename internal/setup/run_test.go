package setup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/23iq/reverse/internal/auth"
	"github.com/23iq/reverse/internal/config"
)

type recordingRunner struct {
	commands []Command
}

func (runner *recordingRunner) Run(_ context.Context, command Command) (string, error) {
	runner.commands = append(runner.commands, command)
	if command.Name == "systemctl" && len(command.Args) >= 2 && command.Args[0] == "is-active" {
		return "inactive\n", errors.New("exit status 3")
	}
	if command.Name == "systemctl" && len(command.Args) >= 2 && command.Args[0] == "is-enabled" {
		return "disabled\n", errors.New("exit status 1")
	}
	if command.Name == "docker" && len(command.Args) >= 2 &&
		command.Args[0] == "container" && command.Args[1] == "inspect" {
		return "Error: No such container", errors.New("exit status 1")
	}
	return "", nil
}

type readinessStub struct {
	err         error
	calls       int
	hasDeadline bool
	address     string
}

func (checker *readinessStub) Wait(ctx context.Context, address string) error {
	checker.calls++
	_, checker.hasDeadline = ctx.Deadline()
	checker.address = address
	return checker.err
}

type statefulSetupRunner struct {
	commands          []Command
	services          map[string]serviceState
	containerExists   bool
	containerRunning  bool
	failDockerRun     bool
	failCaddyReloads  int
	successfulReloads int
	failedReloads     int
}

func (runner *statefulSetupRunner) Run(_ context.Context, command Command) (string, error) {
	runner.commands = append(runner.commands, command)
	if command.Name == "systemctl" && len(command.Args) >= 2 {
		action := command.Args[0]
		service := command.Args[len(command.Args)-1]
		state := runner.services[service]
		switch action {
		case "is-active":
			if state.active {
				return "active\n", nil
			}
			return "inactive\n", errors.New("exit status 3")
		case "is-enabled":
			if state.enabled {
				return "enabled\n", nil
			}
			return "disabled\n", errors.New("exit status 1")
		case "enable":
			state.enabled = true
			if len(command.Args) >= 3 && command.Args[1] == "--now" {
				state.active = true
			}
			runner.services[service] = state
			return "", nil
		case "disable":
			state.enabled = false
			runner.services[service] = state
			return "", nil
		case "start":
			state.active = true
			runner.services[service] = state
			return "", nil
		case "stop":
			state.active = false
			runner.services[service] = state
			return "", nil
		case "reload":
			if service == "caddy" && runner.failCaddyReloads > 0 {
				runner.failCaddyReloads--
				runner.failedReloads++
				return "", errors.New("injected Caddy reload failure")
			}
			runner.successfulReloads++
			return "", nil
		}
	}
	if command.Name == "docker" && len(command.Args) >= 1 {
		switch command.Args[0] {
		case "container":
			if len(command.Args) >= 2 && command.Args[1] == "inspect" {
				if !runner.containerExists {
					return "Error: No such container", errors.New("exit status 1")
				}
				return "true|" + map[bool]string{true: "true", false: "false"}[runner.containerRunning], nil
			}
		case "stop":
			runner.containerRunning = false
		case "start":
			runner.containerExists = true
			runner.containerRunning = true
		case "run":
			runner.containerExists = true
			runner.containerRunning = true
			if runner.failDockerRun {
				return "", errors.New("injected Docker run failure after container creation")
			}
		case "rm":
			if len(command.Args) >= 2 && command.Args[len(command.Args)-1] == "reverse-server" {
				runner.containerExists = false
				runner.containerRunning = false
			}
		}
	}
	return "", nil
}

func setupTestOptions(root, source string, runner Runner) Options {
	return Options{
		Domain:         "edge.example.com",
		Password:       "setup test password",
		RootDir:        root,
		SourceDir:      source,
		PackageManager: PackageManagerAPT,
		Runner:         runner,
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP("203.0.113.20")},
		}},
		PublicIPSource: staticPublicIPs{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.20"),
		}},
		PortChecker:      occupiedPortChecker{},
		ReadinessChecker: &readinessStub{},
		EffectiveUID:     func() int { return 0 },
		LookPath: func(string) (string, error) {
			return "/usr/bin/caddy", nil
		},
	}
}

func writeSetupFixture(t *testing.T, root, source string, caddyData []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestCertificate(t, root, "edge.example.com")
	caddyPath := filepath.Join(root, "etc/caddy/Caddyfile")
	if err := os.MkdirAll(filepath.Dir(caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caddyPath, caddyData, 0o644); err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(root, "etc/reverse/server.json")
	if err := os.MkdirAll(filepath.Dir(serverPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverPath, []byte("{\"domain\":\"edge.example.com\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commandPosition(commands []Command, name string, args ...string) int {
	for index, command := range commands {
		if command.Name != name || len(command.Args) < len(args) {
			continue
		}
		match := true
		for argIndex, arg := range args {
			if command.Args[argIndex] != arg {
				match = false
				break
			}
		}
		if match {
			return index
		}
	}
	return -1
}

func lastCommandPosition(commands []Command, name string, args ...string) int {
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		if command.Name != name || len(command.Args) < len(args) {
			continue
		}
		match := true
		for argIndex, arg := range args {
			if command.Args[argIndex] != arg {
				match = false
				break
			}
		}
		if match {
			return index
		}
	}
	return -1
}

func TestRerunBuildsImageBeforeCutoverWithoutStoppingCaddy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	writeSetupFixture(t, root, source, renderCaddyfile("edge.example.com", root))
	runner := &statefulSetupRunner{
		services: map[string]serviceState{
			"caddy":               {active: true, enabled: true},
			"docker":              {active: true, enabled: true},
			"certbot.timer":       {active: true, enabled: true},
			"certbot-renew.timer": {},
		},
		containerExists:  true,
		containerRunning: true,
	}

	if err := Run(context.Background(), setupTestOptions(root, source, runner), nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	build := commandPosition(runner.commands, "docker", "build")
	stopContainer := commandPosition(runner.commands, "docker", "stop", "reverse-server")
	if build < 0 || stopContainer < 0 || build >= stopContainer {
		t.Fatalf("Docker build must precede cutover stop; build=%d stop=%d commands=%#v", build, stopContainer, runner.commands)
	}
	if stopCaddy := commandPosition(runner.commands, "systemctl", "stop", "caddy"); stopCaddy >= 0 {
		t.Fatalf("reusable certificate rerun stopped Caddy at command %d", stopCaddy)
	}
}

func TestReloadFailureRestoresFilesAndSystemdEnablement(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	oldCaddy := append(renderCaddyfile("edge.example.com", root), []byte("# rollback sentinel\n")...)
	writeSetupFixture(t, root, source, oldCaddy)
	runner := &statefulSetupRunner{
		services: map[string]serviceState{
			"caddy":               {active: true, enabled: false},
			"docker":              {active: true, enabled: false},
			"certbot.timer":       {},
			"certbot-renew.timer": {},
		},
		containerExists:  true,
		containerRunning: true,
		failCaddyReloads: 1,
	}

	err := Run(context.Background(), setupTestOptions(root, source, runner), nil)
	if err == nil || !strings.Contains(err.Error(), "Reloading Caddy") {
		t.Fatalf("Run() error = %v", err)
	}

	gotCaddy, readErr := os.ReadFile(filepath.Join(root, "etc/caddy/Caddyfile"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(gotCaddy) != string(oldCaddy) {
		t.Fatalf("Caddyfile was not restored:\n%s", gotCaddy)
	}
	if runner.failedReloads != 1 || runner.successfulReloads != 1 {
		t.Fatalf("Caddy reloads: failed=%d successful=%d, want one of each", runner.failedReloads, runner.successfulReloads)
	}
	for _, service := range []string{"caddy", "docker"} {
		state := runner.services[service]
		if !state.active || state.enabled {
			t.Fatalf("%s state after rollback = %+v, want active and disabled", service, state)
		}
	}
	if state := runner.services["certbot.timer"]; state.active || state.enabled {
		t.Fatalf("certbot.timer state after rollback = %+v, want inactive and disabled", state)
	}
}

func TestReadinessFailureRollsBackNewContainer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	writeSetupFixture(t, root, source, renderCaddyfile("edge.example.com", root))
	runner := &statefulSetupRunner{
		services: map[string]serviceState{
			"caddy":               {active: true, enabled: true},
			"docker":              {active: true, enabled: true},
			"certbot.timer":       {active: true, enabled: true},
			"certbot-renew.timer": {},
		},
		containerExists:  true,
		containerRunning: true,
	}
	probe := &readinessStub{err: errors.New("injected health failure")}
	options := setupTestOptions(root, source, runner)
	options.ReadinessChecker = probe

	err := Run(context.Background(), options, nil)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Run() error = %v", err)
	}
	if probe.calls != 1 || !probe.hasDeadline || probe.address != "127.0.0.1:8787" {
		t.Fatalf("readiness probe = %+v", probe)
	}
	remove := commandPosition(runner.commands, "docker", "rm", "-f", "reverse-server")
	restoreRename := lastCommandPosition(runner.commands, "docker", "rename")
	if remove < 0 || restoreRename <= remove {
		t.Fatalf("new container was not removed before backup restore; remove=%d rename=%d", remove, restoreRename)
	}
	if !runner.containerExists || !runner.containerRunning {
		t.Fatalf("previous container was not restarted: exists=%v running=%v", runner.containerExists, runner.containerRunning)
	}
}

func TestReadinessFailureWithoutPreviousContainerRemovesNewContainer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	writeSetupFixture(t, root, source, renderCaddyfile("edge.example.com", root))
	runner := &statefulSetupRunner{
		services: map[string]serviceState{
			"caddy":               {active: true, enabled: true},
			"docker":              {active: true, enabled: true},
			"certbot.timer":       {active: true, enabled: true},
			"certbot-renew.timer": {},
		},
	}
	probe := &readinessStub{err: errors.New("injected health failure")}
	options := setupTestOptions(root, source, runner)
	options.ReadinessChecker = probe

	err := Run(context.Background(), options, nil)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Run() error = %v", err)
	}
	if remove := commandPosition(runner.commands, "docker", "rm", "-f", "reverse-server"); remove < 0 {
		t.Fatalf("new container was not removed: %#v", runner.commands)
	}
	if runner.containerExists || runner.containerRunning {
		t.Fatalf("failed container remains: exists=%v running=%v", runner.containerExists, runner.containerRunning)
	}
}

func TestDockerRunFailureRemovesPartiallyCreatedContainer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	writeSetupFixture(t, root, source, renderCaddyfile("edge.example.com", root))
	runner := &statefulSetupRunner{
		services: map[string]serviceState{
			"caddy":               {active: true, enabled: true},
			"docker":              {active: true, enabled: true},
			"certbot.timer":       {active: true, enabled: true},
			"certbot-renew.timer": {},
		},
		containerExists:  true,
		containerRunning: true,
		failDockerRun:    true,
	}

	err := Run(context.Background(), setupTestOptions(root, source, runner), nil)
	if err == nil || !strings.Contains(err.Error(), "Starting the reverse server") {
		t.Fatalf("Run() error = %v", err)
	}
	remove := commandPosition(runner.commands, "docker", "rm", "-f", "reverse-server")
	restoreRename := lastCommandPosition(runner.commands, "docker", "rename")
	if remove < 0 || restoreRename <= remove {
		t.Fatalf("partial container was not removed before backup restore; remove=%d rename=%d", remove, restoreRename)
	}
}

func TestRunWithInjectedSystemDependencies(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestCertificate(t, root, "edge.example.com")

	runner := &recordingRunner{}
	err := Run(context.Background(), Options{
		Domain:         "edge.example.com",
		Password:       "a password longer than bcrypt's old input ceiling " + strings.Repeat("x", 100),
		RootDir:        root,
		SourceDir:      source,
		PackageManager: PackageManagerAPT,
		Runner:         runner,
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP("203.0.113.20")},
		}},
		PublicIPSource: staticPublicIPs{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.20"),
		}},
		PortChecker:      occupiedPortChecker{},
		ReadinessChecker: &readinessStub{},
		EffectiveUID:     func() int { return 0 },
		LookPath: func(string) (string, error) {
			return "", errors.New("not installed yet")
		},
	}, nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	serverPath := filepath.Join(root, "etc/reverse/server.json")
	server, err := config.LoadServer(serverPath)
	if err != nil {
		t.Fatalf("load generated server config: %v", err)
	}
	if server.DirectBind != "127.0.0.1" {
		t.Fatalf("DirectBind = %q", server.DirectBind)
	}
	password := "a password longer than bcrypt's old input ceiling " + strings.Repeat("x", 100)
	if err := auth.ComparePassword(server.PasswordHash, password); err != nil {
		t.Fatalf("generated password hash does not verify: %v", err)
	}
	info, err := os.Stat(serverPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("server config mode = %o", info.Mode().Perm())
	}

	for _, relative := range []string{
		"etc/letsencrypt/renewal-hooks/pre/reverse-caddy",
		"etc/letsencrypt/renewal-hooks/deploy/reverse-caddy",
		"etc/letsencrypt/renewal-hooks/post/reverse-caddy",
	} {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("stat %s: %v", relative, err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Fatalf("%s mode = %o", relative, info.Mode().Perm())
		}
	}

	for _, command := range runner.commands {
		display := command.String()
		if strings.Contains(display, password) || strings.Contains(display, server.PasswordHash) {
			t.Fatalf("command exposes credentials: %s", display)
		}
	}
}

func TestCertificateUsableRequiresMatchingPrivateKey(t *testing.T) {
	t.Parallel()

	const domain = "edge.example.com"
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	writeTestCertificate(t, firstRoot, domain)
	writeTestCertificate(t, secondRoot, domain)

	certificatePath := filepath.Join(firstRoot, "etc/letsencrypt/live", domain, "fullchain.pem")
	privateKeyPath := filepath.Join(firstRoot, "etc/letsencrypt/live", domain, "privkey.pem")
	if !certificateUsable(certificatePath, privateKeyPath, domain, time.Now()) {
		t.Fatal("matching certificate and key were rejected")
	}

	otherKey, err := os.ReadFile(filepath.Join(secondRoot, "etc/letsencrypt/live", domain, "privkey.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateKeyPath, otherKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if certificateUsable(certificatePath, privateKeyPath, domain, time.Now()) {
		t.Fatal("mismatched certificate and private key were accepted")
	}
}

func TestRunRefusesExistingUnmanagedCaddyfile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caddyPath := filepath.Join(root, "etc/caddy/Caddyfile")
	if err := os.MkdirAll(filepath.Dir(caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caddyPath, []byte("unrelated.example {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &recordingRunner{}
	err := Run(context.Background(), Options{
		Domain:         "edge.example.com",
		Password:       "secret",
		RootDir:        root,
		SourceDir:      source,
		PackageManager: PackageManagerAPT,
		Runner:         runner,
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP("203.0.113.20")},
		}},
		PublicIPSource: staticPublicIPs{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.20"),
		}},
		PortChecker:  occupiedPortChecker{},
		EffectiveUID: func() int { return 0 },
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "not managed by reverse") {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands were run before refusing the unmanaged Caddyfile: %#v", runner.commands)
	}
	data, readErr := os.ReadFile(caddyPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "unrelated.example {\n}\n" {
		t.Fatalf("unmanaged Caddyfile was changed: %q", data)
	}
}

func writeTestCertificate(t *testing.T, root, domain string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{domain},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyData, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "etc/letsencrypt/live", domain)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificate,
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "privkey.pem"), pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyData,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
}
