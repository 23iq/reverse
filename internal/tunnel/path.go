package tunnel

import (
	"errors"
	"net/url"
	pathpkg "path"
	"strings"
)

func validateTunnelPath(tunnelPath string) error {
	if tunnelPath == "" {
		return errors.New("tunnel path is required")
	}
	if !strings.HasPrefix(tunnelPath, "/") {
		return errors.New("tunnel path must start with '/'")
	}
	if tunnelPath == "/" {
		return errors.New("tunnel path cannot be the root path")
	}
	if pathpkg.Clean(tunnelPath) != tunnelPath {
		return errors.New("tunnel path must be clean and cannot end with '/'")
	}
	if strings.ContainsAny(tunnelPath, "{}\\") {
		return errors.New("tunnel path must be a literal URL path")
	}

	parsed, err := url.ParseRequestURI(tunnelPath)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != tunnelPath {
		return errors.New("tunnel path must be an unescaped path without a query or fragment")
	}
	return nil
}
