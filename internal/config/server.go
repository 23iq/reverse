package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

const DefaultServerPath = "/etc/reverse/server.json"

type Server struct {
	Domain       string `json:"domain"`
	Listen       string `json:"listen"`
	DirectBind   string `json:"direct_bind"`
	PasswordHash string `json:"password_hash"`
}

func (s *Server) ApplyDefaults() {
	if s.Listen == "" {
		s.Listen = "127.0.0.1:8787"
	}
	if s.DirectBind == "" {
		s.DirectBind = "127.0.0.1"
	}
	s.Domain = strings.ToLower(strings.TrimSpace(s.Domain))
}

func (s Server) Validate() error {
	if s.Domain == "" {
		return errors.New("domain is required")
	}
	if strings.ContainsAny(s.Domain, " \t\r\n/:") {
		return errors.New("domain must be a hostname without a scheme, path, or port")
	}
	if net.ParseIP(s.Domain) != nil || !strings.Contains(s.Domain, ".") {
		return errors.New("domain must be a fully qualified hostname")
	}
	listenHost, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if ip := net.ParseIP(listenHost); ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must use a loopback IP")
	}
	if ip := net.ParseIP(s.DirectBind); ip == nil {
		return errors.New("direct_bind must be an IP address")
	} else if !ip.IsLoopback() {
		return errors.New("direct_bind must use a loopback IP")
	}
	if strings.TrimSpace(s.PasswordHash) == "" {
		return errors.New("password_hash is required")
	}
	return nil
}

func LoadServer(path string) (Server, error) {
	if path == "" {
		path = DefaultServerPath
	}
	file, err := os.Open(path)
	if err != nil {
		return Server{}, fmt.Errorf("open server config: %w", err)
	}
	defer file.Close()

	var cfg Server
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Server{}, fmt.Errorf("decode server config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return Server{}, fmt.Errorf("invalid server config: %w", err)
	}
	return cfg, nil
}
