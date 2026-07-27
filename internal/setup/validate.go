package setup

import (
	"errors"
	"fmt"
	"net/mail"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/23iq/reverse/internal/auth"
)

var (
	domainLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	containerPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

func Validate(opts Options) error {
	_, err := normalizeOptions(opts)
	return err
}

func normalizeOptions(opts Options) (Options, error) {
	opts.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(opts.Domain), "."))
	if err := validateDomain(opts.Domain); err != nil {
		return Options{}, err
	}
	if err := auth.ValidatePassword(opts.Password); err != nil {
		return Options{}, err
	}
	if opts.Email != "" {
		address, err := mail.ParseAddress(opts.Email)
		if err != nil || address.Address != opts.Email {
			return Options{}, errors.New("email must be a plain valid email address")
		}
	}

	if opts.RootDir == "" {
		opts.RootDir = string(filepath.Separator)
	}
	if !filepath.IsAbs(opts.RootDir) {
		return Options{}, errors.New("root directory must be an absolute path")
	}
	opts.RootDir = filepath.Clean(opts.RootDir)

	if opts.SourceDir != "" {
		sourceDir, err := filepath.Abs(opts.SourceDir)
		if err != nil {
			return Options{}, fmt.Errorf("resolve source directory: %w", err)
		}
		opts.SourceDir = filepath.Clean(sourceDir)
	}

	if opts.ServerImage == "" {
		opts.ServerImage = "reverse-server:local"
	}
	if strings.HasPrefix(opts.ServerImage, "-") ||
		strings.TrimSpace(opts.ServerImage) != opts.ServerImage ||
		strings.ContainsAny(opts.ServerImage, "\r\n\t ") {
		return Options{}, errors.New("server image is invalid")
	}
	if opts.ContainerName == "" {
		opts.ContainerName = "reverse-server"
	}
	if !containerPattern.MatchString(opts.ContainerName) {
		return Options{}, errors.New("container name contains invalid characters")
	}
	switch opts.PackageManager {
	case "", PackageManagerAPT, PackageManagerDNF, PackageManagerPacman:
	default:
		return Options{}, fmt.Errorf("unsupported package manager %q", opts.PackageManager)
	}
	return opts, nil
}

func validateDomain(domain string) error {
	if domain == "" {
		return errors.New("domain must not be empty")
	}
	if len(domain) > 253 {
		return errors.New("domain is too long")
	}
	if strings.ContainsAny(domain, "/:@* \t\r\n") {
		return errors.New("domain must be a hostname without a scheme, wildcard, path, or port")
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return errors.New("domain must be a fully qualified hostname")
	}
	for _, label := range labels {
		if !domainLabelPattern.MatchString(label) {
			return fmt.Errorf("domain label %q is invalid; use an ASCII or punycode hostname", label)
		}
	}
	return nil
}
