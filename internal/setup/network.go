package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type bindPortChecker struct {
	ListenConfig net.ListenConfig
}

func (checker bindPortChecker) Check(ctx context.Context, address string) error {
	listener, err := checker.ListenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("address %s is already in use or cannot be bound: %w", address, err)
	}
	return listener.Close()
}

type httpReadinessChecker struct {
	Client     *http.Client
	RetryDelay time.Duration
}

func (checker httpReadinessChecker) Wait(ctx context.Context, address string) error {
	client := checker.Client
	if client == nil {
		client = &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				Proxy:       nil,
				DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			},
		}
	}
	retryDelay := checker.RetryDelay
	if retryDelay <= 0 {
		retryDelay = 150 * time.Millisecond
	}
	endpoint := "http://" + address + "/_reverse/health"
	var lastErr error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			var health struct {
				Healthy bool `json:"healthy"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health)
			closeErr := response.Body.Close()
			switch {
			case response.StatusCode != http.StatusOK:
				lastErr = fmt.Errorf("health endpoint returned %s", response.Status)
			case decodeErr != nil:
				lastErr = fmt.Errorf("decode health response: %w", decodeErr)
			case closeErr != nil:
				lastErr = fmt.Errorf("close health response: %w", closeErr)
			case !health.Healthy:
				lastErr = errors.New("health endpoint reported unhealthy")
			default:
				return nil
			}
		} else {
			lastErr = err
		}

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr != nil {
				return fmt.Errorf("%w (last probe: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type publicIPDetector struct {
	Client *http.Client
}

func (detector publicIPDetector) PublicIPs(ctx context.Context) ([]netip.Addr, error) {
	seen := make(map[netip.Addr]struct{})
	addresses, interfaceErr := net.InterfaceAddrs()
	if interfaceErr == nil {
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if isPublicIP(ip) {
				seen[ip] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		client := detector.Client
		if client == nil {
			client = &http.Client{Timeout: 4 * time.Second}
		}
		for _, endpoint := range []string{"https://api.ipify.org", "https://api6.ipify.org"} {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				continue
			}
			response, err := client.Do(request)
			if err != nil {
				continue
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 128))
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK {
				continue
			}
			ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
			if err == nil && isPublicIP(ip.Unmap()) {
				seen[ip.Unmap()] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		if interfaceErr != nil {
			return nil, fmt.Errorf("inspect network interfaces: %w", interfaceErr)
		}
		return nil, errors.New("could not determine a public VPS address")
	}
	result := make([]netip.Addr, 0, len(seen))
	for ip := range seen {
		result = append(result, ip)
	}
	return result, nil
}

func isPublicIP(ip netip.Addr) bool {
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}
