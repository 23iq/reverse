package setup

import (
	"strings"
	"testing"
)

func TestNormalizeOptions(t *testing.T) {
	t.Parallel()

	longPassword := strings.Repeat("correct horse battery staple ", 20)
	options, err := normalizeOptions(Options{
		Domain:      "  Tunnel.Example.COM. ",
		Password:    longPassword,
		RootDir:     t.TempDir(),
		SourceDir:   ".",
		ServerImage: "registry.example.com/reverse:test",
	})
	if err != nil {
		t.Fatalf("normalizeOptions() error = %v", err)
	}
	if options.Domain != "tunnel.example.com" {
		t.Fatalf("Domain = %q, want tunnel.example.com", options.Domain)
	}
	if options.Password != longPassword {
		t.Fatal("normalizeOptions changed password bytes")
	}
	if options.ContainerName != "reverse-server" {
		t.Fatalf("ContainerName = %q", options.ContainerName)
	}

	embeddedOptions, err := normalizeOptions(Options{
		Domain:   "tunnel.example.com",
		Password: "secret",
		RootDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("normalizeOptions(embedded context) error = %v", err)
	}
	if embeddedOptions.SourceDir != "" {
		t.Fatalf("SourceDir = %q, want embedded context sentinel", embeddedOptions.SourceDir)
	}
}

func TestValidateRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options Options
	}{
		{name: "empty password", options: Options{Domain: "example.com"}},
		{name: "newline password", options: Options{Domain: "example.com", Password: "secret\nnext"}},
		{name: "nul password", options: Options{Domain: "example.com", Password: "secret\x00next"}},
		{name: "scheme", options: Options{Domain: "https://example.com", Password: "secret"}},
		{name: "wildcard", options: Options{Domain: "*.example.com", Password: "secret"}},
		{name: "single label", options: Options{Domain: "localhost", Password: "secret"}},
		{name: "unicode hostname", options: Options{Domain: "täst.example", Password: "secret"}},
		{name: "relative root", options: Options{Domain: "example.com", Password: "secret", RootDir: "staging"}},
		{name: "bad email", options: Options{Domain: "example.com", Password: "secret", Email: "Name <ops@example.com>"}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := Validate(test.options); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}
