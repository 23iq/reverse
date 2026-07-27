package setup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type execRunner struct{}

func (execRunner) Run(ctx context.Context, command Command) (string, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = command.Dir
	if len(command.Env) != 0 {
		cmd.Env = append(os.Environ(), command.Env...)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(output.String())
		if len(message) > 4096 {
			message = message[len(message)-4096:]
		}
		if message == "" {
			return "", fmt.Errorf("%s: %w", command.String(), err)
		}
		return output.String(), fmt.Errorf("%s: %w: %s", command.String(), err, message)
	}
	return output.String(), nil
}

func (c Command) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, shellQuote(c.Name))
	for _, arg := range c.Args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>()*?[]{}!") {
		return value
	}
	return strconv.Quote(value)
}

func installCommands(manager PackageManager) ([]Command, error) {
	switch manager {
	case PackageManagerAPT:
		return []Command{
			{Name: "apt-get", Args: []string{"update"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}},
			{Name: "apt-get", Args: []string{"install", "-y", "docker.io", "caddy", "certbot", "acl"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}},
		}, nil
	case PackageManagerDNF:
		return []Command{
			{Name: "dnf", Args: []string{"install", "-y", "moby-engine", "caddy", "certbot", "acl"}},
		}, nil
	case PackageManagerPacman:
		return []Command{
			{Name: "pacman", Args: []string{"-Sy", "--noconfirm", "--needed", "docker", "caddy", "certbot", "acl"}},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported package manager %q", manager)
	}
}

func certbotCommand(opts Options) Command {
	args := []string{
		"certonly",
		"--standalone",
		"--non-interactive",
		"--agree-tos",
		"--preferred-challenges", "http",
		"--cert-name", opts.Domain,
		"--force-renewal",
		"--domain", opts.Domain,
	}
	if opts.Email == "" {
		args = append(args, "--register-unsafely-without-email")
	} else {
		args = append(args, "--email", opts.Email)
	}
	if opts.RootDir != string(os.PathSeparator) {
		args = append(args,
			"--config-dir", rootPath(opts.RootDir, "/etc/letsencrypt"),
			"--work-dir", rootPath(opts.RootDir, "/var/lib/letsencrypt"),
			"--logs-dir", rootPath(opts.RootDir, "/var/log/letsencrypt"),
		)
	}
	return Command{Name: "certbot", Args: args}
}
