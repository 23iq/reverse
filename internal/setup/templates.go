package setup

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/23iq/reverse/internal/config"
)

const caddyManagedMarker = "# Managed by reverse setup."

func renderServerConfig(domain, passwordHash string) ([]byte, error) {
	server := config.Server{
		Domain:       domain,
		Listen:       "127.0.0.1:8787",
		DirectBind:   "127.0.0.1",
		PasswordHash: passwordHash,
	}
	if err := server.Validate(); err != nil {
		return nil, fmt.Errorf("validate generated server config: %w", err)
	}
	data, err := json.MarshalIndent(server, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode server config: %w", err)
	}
	return append(data, '\n'), nil
}

func renderCaddyfile(domain, rootDir string) []byte {
	certificate := rootPath(rootDir, filepath.Join("/etc/letsencrypt/live", domain, "fullchain.pem"))
	privateKey := rootPath(rootDir, filepath.Join("/etc/letsencrypt/live", domain, "privkey.pem"))
	return []byte(fmt.Sprintf(`%s
# Manual changes to this file may be overwritten by reverse --setup.

%s {
	tls %q %q
	encode zstd gzip
	reverse_proxy 127.0.0.1:8787
}
`, caddyManagedMarker, domain, certificate, privateKey))
}

type renewalHook struct {
	path string
	data []byte
}

func renderRenewalHooks(domain, rootDir string) []renewalHook {
	lineage := rootPath(rootDir, filepath.Join("/etc/letsencrypt/live", domain))
	archive := rootPath(rootDir, filepath.Join("/etc/letsencrypt/archive", domain))
	caddyfile := rootPath(rootDir, "/etc/caddy/Caddyfile")
	stateFile := rootPath(rootDir, "/run/reverse-certbot-caddy-stopped")
	hookDirectory := rootPath(rootDir, "/etc/letsencrypt/renewal-hooks")

	pre := []byte(fmt.Sprintf(`#!/bin/sh
set -eu

if grep -Fq %s %s && systemctl is-active --quiet caddy; then
	: > %s
	systemctl stop caddy
fi
`, shellLiteral(caddyManagedMarker), shellLiteral(caddyfile), shellLiteral(stateFile)))

	deploy := []byte(fmt.Sprintf(`#!/bin/sh
set -eu

if [ "${RENEWED_LINEAGE:-}" != %s ]; then
	exit 0
fi

setfacl --modify u:caddy:--x %s
setfacl --modify u:caddy:--x %s %s
setfacl --recursive --modify u:caddy:rX %s %s
setfacl --default --modify u:caddy:rX %s
`, shellLiteral(lineage),
		shellLiteral(rootPath(rootDir, "/etc/letsencrypt")),
		shellLiteral(rootPath(rootDir, "/etc/letsencrypt/live")),
		shellLiteral(rootPath(rootDir, "/etc/letsencrypt/archive")),
		shellLiteral(lineage),
		shellLiteral(archive),
		shellLiteral(archive),
	))

	post := []byte(fmt.Sprintf(`#!/bin/sh
set -eu

if [ -f %s ]; then
	systemctl start caddy
	rm -f %s
fi
`, shellLiteral(stateFile), shellLiteral(stateFile)))

	return []renewalHook{
		{path: filepath.Join(hookDirectory, "pre/reverse-caddy"), data: pre},
		{path: filepath.Join(hookDirectory, "deploy/reverse-caddy"), data: deploy},
		{path: filepath.Join(hookDirectory, "post/reverse-caddy"), data: post},
	}
}

func shellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func rootPath(rootDir, absolutePath string) string {
	if rootDir == "" || rootDir == string(filepath.Separator) {
		return filepath.Clean(absolutePath)
	}
	return filepath.Join(rootDir, filepath.Clean(absolutePath))
}
