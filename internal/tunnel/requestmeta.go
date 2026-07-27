package tunnel

import (
	"net"
	"net/http"
	"strings"
)

func isLoopbackGateway(request *http.Request) bool {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func effectiveRemoteAddr(request *http.Request) string {
	if !isLoopbackGateway(request) {
		return request.RemoteAddr
	}

	for _, candidate := range strings.Split(request.Header.Get("X-Forwarded-For"), ",") {
		candidate = strings.TrimSpace(candidate)
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}
	if candidate := strings.TrimSpace(request.Header.Get("X-Real-IP")); candidate != "" {
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}
	return request.RemoteAddr
}

func effectiveProto(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	if isLoopbackGateway(request) {
		proto := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0]))
		if proto == "http" || proto == "https" {
			return proto
		}
	}
	return "http"
}

func discardUntrustedForwardingHeaders(request *http.Request) {
	if isLoopbackGateway(request) {
		return
	}
	request.Header.Del("Forwarded")
	request.Header.Del("X-Forwarded-For")
	request.Header.Del("X-Forwarded-Host")
	request.Header.Del("X-Forwarded-Proto")
	request.Header.Del("X-Real-IP")
}

func hostOnly(address string) string {
	address = strings.TrimSpace(address)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	if ip := net.ParseIP(address); ip != nil {
		return ip.String()
	}
	return ""
}
