package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServerAppliesDefaults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.json")
	contents := `{
  "domain": "Tunnel.Example.com",
  "password_hash": "$2a$12$example"
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer() error = %v", err)
	}
	if got.Domain != "tunnel.example.com" {
		t.Fatalf("Domain = %q", got.Domain)
	}
	if got.Listen != "127.0.0.1:8787" {
		t.Fatalf("Listen = %q", got.Listen)
	}
	if got.DirectBind != "127.0.0.1" {
		t.Fatalf("DirectBind = %q", got.DirectBind)
	}
}

func TestServerValidation(t *testing.T) {
	t.Parallel()
	valid := Server{
		Domain:       "tunnel.example.com",
		Listen:       "127.0.0.1:8787",
		DirectBind:   "127.0.0.1",
		PasswordHash: "hash",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Server){
		"empty domain":  func(s *Server) { s.Domain = "" },
		"URL domain":    func(s *Server) { s.Domain = "https://example.com" },
		"IP domain":     func(s *Server) { s.Domain = "192.0.2.1" },
		"bad listen":    func(s *Server) { s.Listen = "8787" },
		"public listen": func(s *Server) { s.Listen = "0.0.0.0:8787" },
		"bad bind":      func(s *Server) { s.DirectBind = "all" },
		"public bind":   func(s *Server) { s.DirectBind = "0.0.0.0" },
		"empty hash":    func(s *Server) { s.PasswordHash = "" },
	} {
		cfg := valid
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() unexpectedly succeeded", name)
		}
	}
}
