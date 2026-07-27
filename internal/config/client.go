package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/23iq/reverse/internal/auth"
)

const (
	configDirectoryName = "reverse"
	clientFileName      = "config.json"
)

var ErrNotConfigured = errors.New("reverse is not configured")

// Client contains the credentials and local defaults needed by the reverse
// client. The password is protected by TLS in transit and by file permissions
// at rest.
type Client struct {
	ServerURL string `json:"server_url"`
	Password  string `json:"password"`
	LocalHost string `json:"local_host,omitempty"`
}

// DefaultClientPath returns the per-user configuration path. REVERSE_CONFIG
// can be used by packaging and tests to select another path.
func DefaultClientPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("REVERSE_CONFIG")); path != "" {
		return filepath.Clean(path), nil
	}

	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(root, configDirectoryName, clientFileName), nil
}

func NormalizeServerURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("domain is required")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse domain: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", errors.New("the server URL must use https")
	}
	if parsed.User != nil {
		return "", errors.New("the server URL cannot contain credentials")
	}
	if parsed.Host == "" {
		return "", errors.New("domain is required")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("the server URL cannot contain a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("the server URL cannot contain a path")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("domain is required")
	}
	if strings.ContainsAny(host, " \t\r\n/") {
		return "", errors.New("domain contains invalid characters")
	}

	normalizedHost := host
	if port := parsed.Port(); port != "" {
		normalizedHost = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		normalizedHost = "[" + host + "]"
	}
	return (&url.URL{Scheme: "https", Host: normalizedHost}).String(), nil
}

func (c Client) Validate() error {
	normalized, err := NormalizeServerURL(c.ServerURL)
	if err != nil {
		return err
	}
	if normalized != c.ServerURL {
		return errors.New("server URL is not normalized")
	}
	if err := auth.ValidatePassword(c.Password); err != nil {
		return err
	}
	if c.LocalHost == "" {
		c.LocalHost = "127.0.0.1"
	}
	if ip := net.ParseIP(c.LocalHost); ip == nil && c.LocalHost != "localhost" {
		return errors.New("local host must be localhost or an IP address")
	}
	return nil
}

func LoadClient(path string) (Client, error) {
	if path == "" {
		var err error
		path, err = DefaultClientPath()
		if err != nil {
			return Client{}, err
		}
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Client{}, fmt.Errorf("%w: run reverse --config first", ErrNotConfigured)
	}
	if err != nil {
		return Client{}, fmt.Errorf("open client config: %w", err)
	}
	defer file.Close()

	var cfg Client
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Client{}, fmt.Errorf("decode client config: %w", err)
	}
	if cfg.LocalHost == "" {
		cfg.LocalHost = "127.0.0.1"
	}
	if err := cfg.Validate(); err != nil {
		return Client{}, fmt.Errorf("invalid client config: %w", err)
	}
	return cfg, nil
}

func SaveClient(path string, cfg Client) error {
	if path == "" {
		var err error
		path, err = DefaultClientPath()
		if err != nil {
			return err
		}
	}

	normalized, err := NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return err
	}
	cfg.ServerURL = normalized
	if cfg.LocalHost == "" {
		cfg.LocalHost = "127.0.0.1"
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("secure config directory: %w", err)
	}

	temp, err := os.CreateTemp(parent, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		temp.Close()
		return fmt.Errorf("secure temporary config: %w", err)
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cfg); err != nil {
		temp.Close()
		return fmt.Errorf("write client config: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync client config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close client config: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace client config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("secure client config: %w", err)
	}
	return nil
}
