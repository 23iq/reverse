package setup

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExistingServerDomain(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(`{"domain":"Old.Example.COM."}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := existingServerDomain(path); got != "old.example.com" {
		t.Fatalf("existingServerDomain() = %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"domain":"../../etc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := existingServerDomain(path); got != "" {
		t.Fatalf("existingServerDomain() accepted unsafe domain %q", got)
	}
}

func TestPreviousSetupDomainFallsBackToManagedCaddyfile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	caddyPath := filepath.Join(root, "Caddyfile")
	data := caddyManagedMarker + "\n\nold.example.com {\n\treverse_proxy 127.0.0.1:8787\n}\n"
	if err := os.WriteFile(caddyPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := previousSetupDomain(filepath.Join(root, "missing.json"), caddyPath); got != "old.example.com" {
		t.Fatalf("previousSetupDomain() = %q", got)
	}

	if err := os.WriteFile(caddyPath, []byte("old.example.com {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := previousSetupDomain(filepath.Join(root, "missing.json"), caddyPath); got != "" {
		t.Fatalf("previousSetupDomain() trusted unmanaged Caddyfile: %q", got)
	}
}

func TestRevokeCertificateACLCommandsKeepSharedParents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commands := revokeCertificateACLCommands(Options{RootDir: root}, "old.example.com", false)
	if len(commands) != 2 {
		t.Fatalf("len(commands) = %d, want 2", len(commands))
	}
	archive := filepath.Join(root, "etc/letsencrypt/archive/old.example.com")
	live := filepath.Join(root, "etc/letsencrypt/live/old.example.com")
	if !slices.Contains(commands[0].Args, "--default") ||
		!slices.Contains(commands[0].Args, archive) ||
		!slices.Contains(commands[1].Args, "--recursive") ||
		!slices.Contains(commands[1].Args, archive) ||
		!slices.Contains(commands[1].Args, live) {
		t.Fatalf("unexpected revoke plan: %#v", commands)
	}
}

func TestRevokeCertificateACLCommandsRemoveParentsAfterFreshFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	commands := revokeCertificateACLCommands(Options{RootDir: root}, "edge.example.com", true)
	if len(commands) != 4 {
		t.Fatalf("len(commands) = %d, want 4", len(commands))
	}
	letsencrypt := filepath.Join(root, "etc/letsencrypt")
	live := filepath.Join(letsencrypt, "live")
	archive := filepath.Join(letsencrypt, "archive")
	if !slices.Contains(commands[2].Args, live) ||
		!slices.Contains(commands[2].Args, archive) ||
		!slices.Contains(commands[3].Args, letsencrypt) {
		t.Fatalf("parent ACL cleanup is incomplete: %#v", commands)
	}
}

type failingACLRunner struct {
	count int
}

func (runner *failingACLRunner) Run(context.Context, Command) (string, error) {
	runner.count++
	return "", errors.New("missing ACL")
}

func TestRevokeCertificateACLAttemptsEveryCleanup(t *testing.T) {
	t.Parallel()

	runner := &failingACLRunner{}
	err := revokeCertificateACL(context.Background(), Options{
		RootDir: t.TempDir(),
		Runner:  runner,
	}, "old.example.com", false, nil)
	if err == nil {
		t.Fatal("revokeCertificateACL() error = nil")
	}
	if runner.count != len(revokeCertificateACLCommands(Options{RootDir: "/"}, "old.example.com", false)) {
		t.Fatalf("attempted %d cleanup commands", runner.count)
	}
}

type failBuildRunner struct {
	commands       []Command
	failACLGrantAt int
	aclGrantCount  int
}

func (runner *failBuildRunner) Run(_ context.Context, command Command) (string, error) {
	runner.commands = append(runner.commands, command)
	if command.Name == "systemctl" && len(command.Args) >= 2 {
		switch command.Args[0] {
		case "is-active":
			return "inactive\n", errors.New("exit status 3")
		case "is-enabled":
			return "disabled\n", errors.New("exit status 1")
		}
	}
	if command.Name == "docker" && len(command.Args) >= 2 &&
		command.Args[0] == "container" && command.Args[1] == "inspect" {
		return "Error: No such container", errors.New("exit status 1")
	}
	if command.Name == "docker" && len(command.Args) >= 1 && command.Args[0] == "build" {
		return "", errors.New("injected build failure")
	}
	if command.Name == "setfacl" && slices.Contains(command.Args, "--modify") {
		runner.aclGrantCount++
		if runner.aclGrantCount == runner.failACLGrantAt {
			return "", errors.New("injected ACL grant failure")
		}
	}
	return "", nil
}

func TestPartialFreshACLGrantIsFullyRevoked(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestCertificate(t, root, "edge.example.com")
	runner := &failBuildRunner{failACLGrantAt: 2}
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
		LookPath: func(string) (string, error) {
			return "", errors.New("not installed")
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "injected ACL grant failure") {
		t.Fatalf("Run() error = %v", err)
	}

	revokes := 0
	for _, command := range runner.commands {
		if command.Name == "setfacl" && slices.Contains(command.Args, "--remove") {
			revokes++
		}
	}
	want := len(revokeCertificateACLCommands(Options{RootDir: root}, "edge.example.com", true))
	if revokes != want {
		t.Fatalf("ACL revoke commands = %d, want %d", revokes, want)
	}
}

func TestFreshSetupFailureRevokesNewCertificateACL(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestCertificate(t, root, "edge.example.com")
	runner := &failBuildRunner{}
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
		LookPath: func(string) (string, error) {
			return "", errors.New("not installed")
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "injected build failure") {
		t.Fatalf("Run() error = %v", err)
	}

	revokes := 0
	for _, command := range runner.commands {
		if command.Name == "setfacl" && slices.Contains(command.Args, "--remove") {
			revokes++
		}
	}
	if revokes != len(revokeCertificateACLCommands(Options{RootDir: root}, "edge.example.com", true)) {
		t.Fatalf("ACL revoke commands = %d, want %d", revokes, len(revokeCertificateACLCommands(Options{RootDir: root}, "edge.example.com", true)))
	}
}
