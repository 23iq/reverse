package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// DefaultTunnelPath is the websocket endpoint used by reverse clients.
	DefaultTunnelPath = "/_reverse/tunnel"

	// TokenHeader is accepted in addition to an Authorization bearer token.
	TokenHeader = "X-Reverse-Token"

	// ErrorCodeHeader carries a machine-readable websocket handshake rejection.
	ErrorCodeHeader = "X-Reverse-Error"
)

var (
	ErrServerClosed               = errors.New("tunnel server is closed")
	ErrAuthenticationRejected     = errors.New("tunnel authentication rejected")
	ErrVerificationUnavailable    = errors.New("tunnel verification is temporarily unavailable")
	ErrRateLimited                = errors.New("tunnel handshake rate limited")
	ErrTunnelBusy                 = errors.New("a tunnel session is already active")
	ErrPublicPortInUse            = errors.New("requested public port is already in use")
	ErrPublicPortPermissionDenied = errors.New("permission denied binding requested public port")
	ErrPublicBindFailed           = errors.New("could not bind requested public port")
	ErrNoActiveSession            = errors.New("no tunnel session is active")
	ErrTunnelDisconnected         = errors.New("tunnel connection was lost")

	// ErrSessionActive is kept as the descriptive server-side spelling of
	// ErrTunnelBusy.
	ErrSessionActive = ErrTunnelBusy
)

// VerifyRequest contains the authenticated portion of a tunnel handshake.
// Header is a clone and may safely be retained by the verifier.
type VerifyRequest struct {
	Token      string
	Domain     string
	RemoteAddr string
	PublicPort uint16
	Header     http.Header
}

// VerifyFunc authenticates a client before the public TCP port is reserved and
// before the websocket upgrade is accepted. It must return
// ErrAuthenticationRejected (possibly wrapped) only for invalid credentials;
// all other errors are treated as temporary verifier failures.
type VerifyFunc func(context.Context, VerifyRequest) error

type EventType string

const (
	EventSessionOnline   EventType = "session.online"
	EventSessionOffline  EventType = "session.offline"
	EventRequestStarted  EventType = "request.started"
	EventRequestFinished EventType = "request.finished"
	EventRequestError    EventType = "request.error"
	EventTunnelError     EventType = "tunnel.error"
)

// Event is newline-delimited JSON on the yamux control stream. Fields which do
// not apply to a particular event are omitted.
type Event struct {
	Type       EventType `json:"type"`
	ID         string    `json:"id,omitempty"`
	Time       time.Time `json:"time"`
	Method     string    `json:"method,omitempty"`
	Host       string    `json:"host,omitempty"`
	Path       string    `json:"path,omitempty"`
	RemoteAddr string    `json:"remote_addr,omitempty"`
	Status     int       `json:"status,omitempty"`
	Duration   int64     `json:"duration_ms,omitempty"`
	BytesIn    int64     `json:"bytes_in,omitempty"`
	BytesOut   int64     `json:"bytes_out,omitempty"`
	Error      string    `json:"error,omitempty"`
	PublicPort uint16    `json:"public_port,omitempty"`
}

type StatusState string

const (
	StatusConnecting   StatusState = "connecting"
	StatusOnline       StatusState = "online"
	StatusReconnecting StatusState = "reconnecting"
	StatusOffline      StatusState = "offline"
	StatusStopped      StatusState = "stopped"
)

// Status is delivered to status callbacks. Callback invocations may originate
// from different goroutines and should return promptly.
type Status struct {
	State      StatusState
	At         time.Time
	Attempt    int
	PublicPort uint16
	Error      string
}

// TrafficDirection is expressed relative to the user's local service.
type TrafficDirection string

const (
	TrafficToLocal   TrafficDirection = "to_local"
	TrafficFromLocal TrafficDirection = "from_local"
)

type Traffic struct {
	At             time.Time
	Direction      TrafficDirection
	Bytes          int64
	TotalToLocal   uint64
	TotalFromLocal uint64
}

type EventCallback func(Event)
type StatusCallback func(Status)
type TrafficCallback func(Traffic)

type SessionInfo struct {
	Domain      string    `json:"domain"`
	RemoteAddr  string    `json:"-"`
	PublicPort  uint16    `json:"public_port"`
	ConnectedAt time.Time `json:"connected_at"`
}

type Health struct {
	Healthy bool         `json:"healthy"`
	State   StatusState  `json:"state"`
	Session *SessionInfo `json:"session,omitempty"`
}

// PortInUseError means the requested listener could not be reserved. The
// underlying listener error is retained for logs and errors.Is/errors.As.
type PortInUseError struct {
	Address string
	Err     error
}

func (e *PortInUseError) Error() string {
	return fmt.Sprintf("public port is unavailable at %s: %v", e.Address, e.Err)
}

func (e *PortInUseError) Unwrap() error {
	return e.Err
}

func (e *PortInUseError) Is(target error) bool {
	return target == ErrPublicPortInUse
}

type HandshakeError struct {
	StatusCode int
	Status     string
	Err        error
}

func (e *HandshakeError) Error() string {
	if e.Status != "" {
		return fmt.Sprintf("tunnel handshake rejected: %s", e.Status)
	}
	return fmt.Sprintf("tunnel handshake failed: %v", e.Err)
}

func (e *HandshakeError) Unwrap() error {
	return e.Err
}

// Retryable reports whether reconnecting without changing configuration can
// reasonably succeed.
func (e *HandshakeError) Retryable() bool {
	if errors.Is(e, ErrAuthenticationRejected) ||
		errors.Is(e, ErrPublicPortInUse) ||
		errors.Is(e, ErrPublicPortPermissionDenied) {
		return false
	}
	if errors.Is(e, ErrTunnelBusy) {
		return true
	}
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return false
	default:
		return true
	}
}
