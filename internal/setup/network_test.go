package setup

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type staticResolver struct {
	addresses []net.IPAddr
	err       error
}

func (resolver staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return resolver.addresses, resolver.err
}

type staticPublicIPs struct {
	addresses []netip.Addr
	err       error
}

func (source staticPublicIPs) PublicIPs(context.Context) ([]netip.Addr, error) {
	return source.addresses, source.err
}

func TestVerifyDNS(t *testing.T) {
	t.Parallel()

	domainIP := netip.MustParseAddr("203.0.113.20")
	options := Options{
		Domain: "edge.example.com",
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP(domainIP.String())},
		}},
		PublicIPSource: staticPublicIPs{addresses: []netip.Addr{domainIP}},
	}
	if err := verifyDNS(context.Background(), options, nil); err != nil {
		t.Fatalf("verifyDNS() error = %v", err)
	}

	options.PublicIPSource = staticPublicIPs{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.21")}}
	if err := verifyDNS(context.Background(), options, nil); err == nil {
		t.Fatal("verifyDNS() accepted a mismatched address")
	}
	options.AllowDNSMismatch = true
	if err := verifyDNS(context.Background(), options, nil); err != nil {
		t.Fatalf("verifyDNS() with AllowDNSMismatch error = %v", err)
	}
}

func TestVerifyDNSContinuesWhenPublicDetectionIsUnavailable(t *testing.T) {
	t.Parallel()

	var events []Progress
	options := Options{
		Domain: "edge.example.com",
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP("203.0.113.20")},
		}},
		PublicIPSource: staticPublicIPs{err: errors.New("offline")},
	}
	if err := verifyDNS(context.Background(), options, func(progress Progress) {
		events = append(events, progress)
	}); err != nil {
		t.Fatalf("verifyDNS() error = %v", err)
	}
	if len(events) != 1 || events[0].Status != StatusWarning {
		t.Fatalf("events = %#v, want one warning", events)
	}
}

func TestVerifyDNSRejectsMixedLocalAndForeignRecords(t *testing.T) {
	t.Parallel()

	options := Options{
		Domain: "edge.example.com",
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP("203.0.113.20")},
			{IP: net.ParseIP("2001:db8::99")},
		}},
		PublicIPSource: staticPublicIPs{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.20"),
			netip.MustParseAddr("2001:db8::20"),
		}},
	}
	err := verifyDNS(context.Background(), options, nil)
	if err == nil || !strings.Contains(err.Error(), "2001:db8::99") {
		t.Fatalf("verifyDNS() error = %v, want stale AAAA rejection", err)
	}

	options.AllowDNSMismatch = true
	if err := verifyDNS(context.Background(), options, nil); err != nil {
		t.Fatalf("verifyDNS() with AllowDNSMismatch error = %v", err)
	}
}

func TestVerifyDNSRejectsPrivateRecordBesideVPSAddress(t *testing.T) {
	t.Parallel()

	options := Options{
		Domain: "edge.example.com",
		Resolver: staticResolver{addresses: []net.IPAddr{
			{IP: net.ParseIP("203.0.113.20")},
			{IP: net.ParseIP("10.0.0.5")},
		}},
		PublicIPSource: staticPublicIPs{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.20"),
		}},
	}
	err := verifyDNS(context.Background(), options, nil)
	if err == nil || !strings.Contains(err.Error(), "10.0.0.5") {
		t.Fatalf("verifyDNS() error = %v, want private record rejection", err)
	}
}

type occupiedPortChecker struct {
	address string
}

func (checker occupiedPortChecker) Check(_ context.Context, address string) error {
	if address == checker.address {
		return errors.New("occupied")
	}
	return nil
}

func TestCheckPortsDoesNotRunDestructiveCommands(t *testing.T) {
	t.Parallel()

	err := checkPorts(context.Background(), Options{
		PortChecker: occupiedPortChecker{address: ":443"},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "will not stop its owner") {
		t.Fatalf("checkPorts() error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHTTPReadinessCheckerWaitsForHealthyResponse(t *testing.T) {
	t.Parallel()

	checker := httpReadinessChecker{
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://127.0.0.1:8787/_reverse/health" {
				t.Fatalf("readiness URL = %s", request.URL)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"healthy":true}`)),
				Header:     make(http.Header),
			}, nil
		})},
		RetryDelay: time.Millisecond,
	}
	if err := checker.Wait(context.Background(), "127.0.0.1:8787"); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestHTTPReadinessCheckerHonorsContextDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	checker := httpReadinessChecker{
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Body:       io.NopCloser(strings.NewReader(`{"healthy":false}`)),
				Header:     make(http.Header),
			}, nil
		})},
		RetryDelay: time.Millisecond,
	}
	err := checker.Wait(ctx, "127.0.0.1:8787")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want deadline exceeded", err)
	}
}

type fixedRunner struct {
	output string
	err    error
}

func (runner fixedRunner) Run(context.Context, Command) (string, error) {
	return runner.output, runner.err
}

func TestServiceActiveTreatsMissingUnitAsInactive(t *testing.T) {
	t.Parallel()

	active, err := serviceActive(context.Background(), fixedRunner{
		err: errors.New("systemctl: Unit caddy.service could not be found"),
	}, "caddy")
	if err != nil {
		t.Fatalf("serviceActive() error = %v", err)
	}
	if active {
		t.Fatal("serviceActive() = true for missing unit")
	}
}
