package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInstallCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		manager PackageManager
		binary  string
		pkg     string
	}{
		{PackageManagerAPT, "apt-get", "docker.io"},
		{PackageManagerDNF, "dnf", "moby-engine"},
		{PackageManagerPacman, "pacman", "docker"},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.manager), func(t *testing.T) {
			t.Parallel()
			commands, err := installCommands(test.manager)
			if err != nil {
				t.Fatalf("installCommands() error = %v", err)
			}
			last := commands[len(commands)-1]
			if last.Name != test.binary {
				t.Fatalf("binary = %q, want %q", last.Name, test.binary)
			}
			if !slices.Contains(last.Args, test.pkg) ||
				!slices.Contains(last.Args, "caddy") ||
				!slices.Contains(last.Args, "certbot") ||
				!slices.Contains(last.Args, "acl") {
				t.Fatalf("package args = %#v", last.Args)
			}
		})
	}
}

func TestCertbotCommand(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	command := certbotCommand(Options{
		Domain:   "edge.example.com",
		Password: "must-never-appear",
		RootDir:  root,
	})
	display := command.String()
	for _, expected := range []string{
		"certonly",
		"--standalone",
		"--non-interactive",
		"--register-unsafely-without-email",
		"--cert-name",
		"--force-renewal",
		"edge.example.com",
		filepath.Join(root, "etc/letsencrypt"),
	} {
		if !strings.Contains(display, expected) {
			t.Fatalf("command does not contain %q: %s", expected, display)
		}
	}
	if strings.Contains(display, "must-never-appear") {
		t.Fatal("Certbot command exposes the setup password")
	}
}

func TestDockerRunCommandIncludesRuntimeLimitsAndRequiredCapabilities(t *testing.T) {
	t.Parallel()

	display := dockerRunCommand(Options{
		ServerImage:   "reverse-server:test",
		ContainerName: "reverse-server",
	}, "/etc/reverse/server.json").String()
	for _, expected := range []string{
		"--stop-timeout 15",
		"--pids-limit 256",
		"--log-driver json-file",
		"--log-opt max-size=10m",
		"--log-opt max-file=3",
		"--cap-drop ALL",
		"--cap-add NET_BIND_SERVICE",
		"--cap-add SETGID",
		"--cap-add SETPCAP",
		"--cap-add SETUID",
		"--volume /etc/reverse/server.json:/etc/reverse/server.json:ro,Z",
	} {
		if !strings.Contains(display, expected) {
			t.Fatalf("docker run command does not contain %q: %s", expected, display)
		}
	}
}

type failIfRun struct{}

func (failIfRun) Run(context.Context, Command) (string, error) {
	return "", errors.New("runner must not execute in dry-run mode")
}

func TestDryRunPlansWithoutSideEffects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var events []Progress
	err := Run(context.Background(), Options{
		Domain:         "edge.example.com",
		Password:       "dry-run-password",
		DryRun:         true,
		RootDir:        root,
		SourceDir:      filepath.Join(root, "missing-source-is-valid-in-dry-run"),
		PackageManager: PackageManagerAPT,
		Runner:         failIfRun{},
		EffectiveUID:   func() int { return 1000 },
	}, func(progress Progress) {
		events = append(events, progress)
	})
	if err != nil {
		t.Fatalf("Run(dry-run) error = %v", err)
	}
	if len(events) < 10 {
		t.Fatalf("got %d events, want a complete command plan", len(events))
	}
	var commands strings.Builder
	for _, event := range events {
		commands.WriteString(event.Command)
		commands.WriteByte('\n')
	}
	if strings.Contains(commands.String(), "dry-run-password") {
		t.Fatal("dry-run command plan exposes password")
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); !os.IsNotExist(err) {
		t.Fatalf("dry run created files or returned unexpected error: %v", err)
	}
}
