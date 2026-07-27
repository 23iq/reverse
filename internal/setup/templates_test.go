package setup

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/23iq/reverse/internal/config"
)

func TestRenderServerConfig(t *testing.T) {
	t.Parallel()

	data, err := renderServerConfig("edge.example.com", "bcrypt-hash")
	if err != nil {
		t.Fatalf("renderServerConfig() error = %v", err)
	}
	var server config.Server
	if err := json.Unmarshal(data, &server); err != nil {
		t.Fatalf("generated JSON is invalid: %v", err)
	}
	if server.Domain != "edge.example.com" {
		t.Fatalf("Domain = %q", server.Domain)
	}
	if server.Listen != "127.0.0.1:8787" {
		t.Fatalf("Listen = %q", server.Listen)
	}
	if server.DirectBind != "127.0.0.1" {
		t.Fatalf("DirectBind = %q", server.DirectBind)
	}
	if server.PasswordHash != "bcrypt-hash" {
		t.Fatalf("PasswordHash = %q", server.PasswordHash)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("generated JSON has no final newline")
	}
}

func TestRenderCaddyfile(t *testing.T) {
	t.Parallel()

	data := string(renderCaddyfile("edge.example.com", "/staging"))
	for _, expected := range []string{
		caddyManagedMarker,
		"edge.example.com {",
		`tls "/staging/etc/letsencrypt/live/edge.example.com/fullchain.pem" "/staging/etc/letsencrypt/live/edge.example.com/privkey.pem"`,
		"reverse_proxy 127.0.0.1:8787",
	} {
		if !strings.Contains(data, expected) {
			t.Fatalf("Caddyfile does not contain %q:\n%s", expected, data)
		}
	}
}

func TestRenderRenewalHooks(t *testing.T) {
	t.Parallel()

	hooks := renderRenewalHooks("edge.example.com", "/staging")
	if len(hooks) != 3 {
		t.Fatalf("len(hooks) = %d, want 3", len(hooks))
	}
	if !strings.Contains(hooks[0].path, "/pre/reverse-caddy") ||
		!strings.Contains(hooks[1].path, "/deploy/reverse-caddy") ||
		!strings.Contains(hooks[2].path, "/post/reverse-caddy") {
		t.Fatalf("unexpected hook paths: %#v", hooks)
	}
	pre := string(hooks[0].data)
	if !strings.Contains(pre, caddyManagedMarker) ||
		!strings.Contains(pre, "systemctl stop caddy") {
		t.Fatalf("pre hook does not guard and stop Caddy:\n%s", pre)
	}
	deploy := string(hooks[1].data)
	if !strings.Contains(deploy, `RENEWED_LINEAGE`) ||
		!strings.Contains(deploy, "setfacl --default") {
		t.Fatalf("deploy hook does not maintain certificate ACLs:\n%s", deploy)
	}
	post := string(hooks[2].data)
	start := strings.Index(post, "systemctl start caddy")
	remove := strings.Index(post, "rm -f")
	if start < 0 || remove < 0 || start > remove {
		t.Fatalf("post hook must start Caddy before removing its state file:\n%s", post)
	}
}

func TestShellLiteral(t *testing.T) {
	t.Parallel()

	if got := shellLiteral("a'b"); got != `'a'"'"'b'` {
		t.Fatalf("shellLiteral() = %q", got)
	}
}
