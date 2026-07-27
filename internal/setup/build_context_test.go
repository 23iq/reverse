package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type buildContextInspectingRunner struct {
	*statefulSetupRunner
	directory string
	err       error
}

func TestPrivilegedBuildContextIgnoresInheritedTempDirectory(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "untrusted"))
	if got := buildContextTempRoot(0); got != "/run" {
		t.Fatalf("privileged build context root = %q, want /run", got)
	}
	if got := buildContextTempRoot(1000); got != "" {
		t.Fatalf("unprivileged build context root = %q, want default temp root", got)
	}
}

func (runner *buildContextInspectingRunner) Run(ctx context.Context, command Command) (string, error) {
	if command.Name == "docker" && len(command.Args) > 0 && command.Args[0] == "build" {
		runner.directory = command.Dir
		for _, name := range []string{
			"Dockerfile",
			"assets.go",
			"go.mod",
			"go.sum",
			"cmd/reverse/main.go",
			"cmd/reverse-container-init/main_linux.go",
			"internal/setup/setup.go",
		} {
			info, err := os.Stat(filepath.Join(command.Dir, filepath.FromSlash(name)))
			if err != nil {
				runner.err = fmt.Errorf("stat embedded build asset %s: %w", name, err)
				break
			}
			if !info.Mode().IsRegular() {
				runner.err = fmt.Errorf("embedded build asset %s is not regular", name)
				break
			}
		}
		if runner.err != nil {
			return "", runner.err
		}
	}
	return runner.statefulSetupRunner.Run(ctx, command)
}

func TestRunUsesEmbeddedBuildContextOutsideCheckout(t *testing.T) {
	workingDirectory := t.TempDir()
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	root := t.TempDir()
	writeTestCertificate(t, root, "edge.example.com")
	runner := &buildContextInspectingRunner{
		statefulSetupRunner: &statefulSetupRunner{
			services: map[string]serviceState{
				"caddy":               {},
				"docker":              {},
				"certbot.timer":       {},
				"certbot-renew.timer": {},
			},
		},
	}

	if err := Run(context.Background(), setupTestOptions(root, "", runner), nil); err != nil {
		t.Fatalf("Run() from empty working directory error = %v", err)
	}
	if runner.directory == "" {
		t.Fatal("Docker build did not receive a materialized build context")
	}
	if runner.directory == workingDirectory {
		t.Fatal("Docker build unexpectedly used the current working directory")
	}
	if runner.err != nil {
		t.Fatal(runner.err)
	}
	if _, err := os.Stat(runner.directory); !os.IsNotExist(err) {
		t.Fatalf("temporary build context remains or returned unexpected error: %v", err)
	}
	entries, err := os.ReadDir(workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("setup wrote build assets into the current working directory: %#v", entries)
	}
}

func TestEmbeddedBuildContextBuildsServerImage(t *testing.T) {
	if os.Getenv("REVERSE_RUN_DOCKER_BUILD_INTEGRATION") != "1" {
		t.Skip("set REVERSE_RUN_DOCKER_BUILD_INTEGRATION=1 to run Docker build")
	}

	directory, cleanup, err := prepareBuildContext("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	image := fmt.Sprintf("reverse-server:portable-setup-test-%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("docker", "image", "rm", "--force", image).Run()
	})
	command := exec.Command("docker", "build", "--tag", image, ".")
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build embedded server context: %v\n%s", err, output)
	}
}
