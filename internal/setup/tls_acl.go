package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func previousSetupDomain(serverPath, caddyPath string) string {
	// The active Caddyfile is the certificate consumer that rollback must keep
	// readable, so prefer it if a partially edited installation disagrees with
	// server.json.
	if domain := existingManagedCaddyDomain(caddyPath); domain != "" {
		return domain
	}
	return existingServerDomain(serverPath)
}

func existingServerDomain(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	var stored struct {
		Domain string `json:"domain"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	if err := decoder.Decode(&stored); err != nil {
		return ""
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(stored.Domain), "."))
	if validateDomain(domain) != nil {
		return ""
	}
	return domain
}

func existingManagedCaddyDomain(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	managed := false
	var candidates []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, caddyManagedMarker) {
			managed = true
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "{" {
			domain := strings.ToLower(strings.TrimSuffix(fields[0], "."))
			if validateDomain(domain) == nil {
				candidates = append(candidates, domain)
			}
		}
	}
	if !managed || len(candidates) != 1 {
		return ""
	}
	return candidates[0]
}

func revokeCertificateACLCommands(opts Options, domain string, removeParents bool) []Command {
	letsencrypt := rootPath(opts.RootDir, "/etc/letsencrypt")
	live := rootPath(opts.RootDir, "/etc/letsencrypt/live")
	archive := rootPath(opts.RootDir, "/etc/letsencrypt/archive")
	lineage := rootPath(opts.RootDir, filepath.Join("/etc/letsencrypt/live", domain))
	archiveLineage := rootPath(opts.RootDir, filepath.Join("/etc/letsencrypt/archive", domain))
	commands := []Command{
		{Name: "setfacl", Args: []string{"--default", "--remove", "u:caddy", archiveLineage}},
		{Name: "setfacl", Args: []string{"--recursive", "--remove", "u:caddy", lineage, archiveLineage}},
	}
	if removeParents {
		commands = append(commands,
			Command{Name: "setfacl", Args: []string{"--remove", "u:caddy", live, archive}},
			Command{Name: "setfacl", Args: []string{"--remove", "u:caddy", letsencrypt}},
		)
	}
	return commands
}

func revokeCertificateACL(ctx context.Context, opts Options, domain string, removeParents bool, progress ProgressFunc) error {
	var result error
	for _, command := range revokeCertificateACLCommands(opts, domain, removeParents) {
		if err := execute(ctx, opts.Runner, progress, StageCertificate, "Revoking obsolete Caddy certificate access", command); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
