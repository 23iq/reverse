package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const (
	defaultLocalDialTimeout = 5 * time.Second
	defaultReadyTimeout     = 10 * time.Second
	defaultReconnectMin     = 500 * time.Millisecond
	defaultReconnectMax     = 15 * time.Second
	defaultReconnectFactor  = 1.8
)

type ClientConfig struct {
	ServerURL    string
	TunnelPath   string
	Token        string
	Domain       string
	PublicPort   uint16
	LocalAddress string

	HTTPClient       *http.Client
	Header           http.Header
	LocalDialTimeout time.Duration
	ReadyTimeout     time.Duration

	ReconnectMinDelay time.Duration
	ReconnectMaxDelay time.Duration
	ReconnectFactor   float64
	MaxAttempts       int

	OnEvent   EventCallback
	OnStatus  StatusCallback
	OnTraffic TrafficCallback
}

type Client struct {
	config   ClientConfig
	endpoint string
	meter    trafficMeter
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.ServerURL == "" {
		return nil, errors.New("server URL is required")
	}
	if config.PublicPort == 0 {
		return nil, errors.New("public port must be between 1 and 65535")
	}
	if config.Token == "" && config.Header.Get("Authorization") == "" && config.Header.Get(TokenHeader) == "" {
		return nil, errors.New("tunnel token is required")
	}
	if strings.ContainsAny(config.Token, "\r\n") {
		return nil, errors.New("tunnel token cannot contain a newline")
	}
	if config.LocalAddress == "" {
		return nil, errors.New("local address is required")
	}
	if _, _, err := net.SplitHostPort(config.LocalAddress); err != nil {
		return nil, fmt.Errorf("invalid local address %q: %w", config.LocalAddress, err)
	}
	if config.TunnelPath == "" {
		config.TunnelPath = DefaultTunnelPath
	}
	if config.LocalDialTimeout <= 0 {
		config.LocalDialTimeout = defaultLocalDialTimeout
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = defaultReadyTimeout
	}
	if config.ReconnectMinDelay <= 0 {
		config.ReconnectMinDelay = defaultReconnectMin
	}
	if config.ReconnectMaxDelay <= 0 {
		config.ReconnectMaxDelay = defaultReconnectMax
	}
	if config.ReconnectMaxDelay < config.ReconnectMinDelay {
		return nil, errors.New("reconnect max delay cannot be less than min delay")
	}
	if config.ReconnectFactor <= 1 {
		config.ReconnectFactor = defaultReconnectFactor
	}
	if config.MaxAttempts < 0 {
		return nil, errors.New("max attempts cannot be negative")
	}
	if config.Header == nil {
		config.Header = make(http.Header)
	} else {
		config.Header = config.Header.Clone()
	}

	endpoint, err := buildTunnelURL(config.ServerURL, config.TunnelPath, config.Domain, config.PublicPort)
	if err != nil {
		return nil, err
	}
	client := &Client{config: config, endpoint: endpoint}
	client.meter.callback = config.OnTraffic
	return client, nil
}

// Endpoint returns the fully resolved websocket URL, including non-secret
// handshake query parameters.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// Run maintains the tunnel until ctx is cancelled or a permanent handshake
// error occurs. MaxAttempts == 0 means unlimited reconnect attempts.
func (c *Client) Run(ctx context.Context) error {
	delay := c.config.ReconnectMinDelay
	var lastErr error

	for attempt := 1; c.config.MaxAttempts == 0 || attempt <= c.config.MaxAttempts; attempt++ {
		state := StatusConnecting
		if attempt > 1 {
			state = StatusReconnecting
		}
		callStatus(c.config.OnStatus, Status{
			State:      state,
			At:         time.Now().UTC(),
			Attempt:    attempt,
			PublicPort: c.config.PublicPort,
			Error:      errorString(lastErr),
		})

		err := c.runOnce(ctx, attempt)
		if ctx.Err() != nil {
			callStatus(c.config.OnStatus, Status{
				State:      StatusStopped,
				At:         time.Now().UTC(),
				Attempt:    attempt,
				PublicPort: c.config.PublicPort,
			})
			return nil
		}
		if err == nil {
			return nil
		}
		lastErr = err

		callStatus(c.config.OnStatus, Status{
			State:      StatusOffline,
			At:         time.Now().UTC(),
			Attempt:    attempt,
			PublicPort: c.config.PublicPort,
			Error:      err.Error(),
		})
		var handshakeErr *HandshakeError
		if errors.As(err, &handshakeErr) && !handshakeErr.Retryable() {
			return err
		}
		if c.config.MaxAttempts > 0 && attempt >= c.config.MaxAttempts {
			return err
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			callStatus(c.config.OnStatus, Status{
				State:      StatusStopped,
				At:         time.Now().UTC(),
				Attempt:    attempt,
				PublicPort: c.config.PublicPort,
			})
			return nil
		case <-timer.C:
		}

		delay = time.Duration(float64(delay) * c.config.ReconnectFactor)
		if delay > c.config.ReconnectMaxDelay {
			delay = c.config.ReconnectMaxDelay
		}
	}

	return lastErr
}

// RunOnce establishes one session and returns when it disconnects. It does not
// retry and is mainly useful to supervisors and tests.
func (c *Client) RunOnce(ctx context.Context) error {
	callStatus(c.config.OnStatus, Status{
		State:      StatusConnecting,
		At:         time.Now().UTC(),
		Attempt:    1,
		PublicPort: c.config.PublicPort,
	})
	return c.runOnce(ctx, 1)
}

func (c *Client) runOnce(ctx context.Context, attempt int) error {
	header := c.config.Header.Clone()
	if header.Get("Authorization") == "" && header.Get(TokenHeader) == "" {
		header.Set(TokenHeader, "v1."+base64.RawURLEncoding.EncodeToString([]byte(c.config.Token)))
	}
	socket, response, err := websocket.Dial(ctx, c.endpoint, &websocket.DialOptions{
		HTTPClient: c.config.HTTPClient,
		HTTPHeader: header,
	})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return classifyHandshakeError(response, err)
	}

	socketConn := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
	muxConfig := yamux.DefaultConfig()
	muxConfig.LogOutput = io.Discard
	muxSession, err := yamux.Client(socketConn, muxConfig)
	if err != nil {
		_ = socketConn.Close()
		return err
	}
	defer func() {
		_ = muxSession.Close()
		_ = socketConn.Close()
	}()

	controlConn, err := muxSession.Open()
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	defer controlConn.Close()

	ready := make(chan struct{})
	controlErrors := make(chan error, 1)
	var readyOnce sync.Once
	go func() {
		decoder := json.NewDecoder(controlConn)
		for {
			var event Event
			if err := decoder.Decode(&event); err != nil {
				controlErrors <- err
				_ = muxSession.Close()
				return
			}
			if event.Type == EventSessionOnline {
				readyOnce.Do(func() { close(ready) })
			}
			callEvent(c.config.OnEvent, event)
		}
	}()

	timer := time.NewTimer(c.config.ReadyTimeout)
	select {
	case <-ready:
		stopTimer(timer)
	case err := <-controlErrors:
		stopTimer(timer)
		return fmt.Errorf("control stream closed before ready: %w", err)
	case <-timer.C:
		return errors.New("timed out waiting for server readiness")
	case <-ctx.Done():
		stopTimer(timer)
		return ctx.Err()
	}

	callStatus(c.config.OnStatus, Status{
		State:      StatusOnline,
		At:         time.Now().UTC(),
		Attempt:    attempt,
		PublicPort: c.config.PublicPort,
	})

	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = muxSession.Close()
		case <-sessionDone:
		}
	}()

	var streams sync.WaitGroup
	defer func() {
		_ = muxSession.Close()
		streams.Wait()
	}()
	for {
		stream, acceptErr := muxSession.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case controlErr := <-controlErrors:
				return fmt.Errorf("%w: control stream: %v", ErrTunnelDisconnected, controlErr)
			default:
			}
			return fmt.Errorf("%w: %v", ErrTunnelDisconnected, acceptErr)
		}

		streams.Add(1)
		go func(conn net.Conn) {
			defer streams.Done()
			c.handleStream(ctx, conn)
		}(stream)
	}
}

func (c *Client) handleStream(ctx context.Context, stream net.Conn) {
	defer stream.Close()

	dialer := net.Dialer{Timeout: c.config.LocalDialTimeout}
	local, err := dialer.DialContext(ctx, "tcp", c.config.LocalAddress)
	if err != nil {
		callEvent(c.config.OnEvent, Event{
			Type:  EventTunnelError,
			Time:  time.Now().UTC(),
			Error: fmt.Sprintf("connect local service at %s: %v", c.config.LocalAddress, err),
		})
		return
	}
	defer local.Close()

	results := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(&trafficWriter{
			Writer:    local,
			meter:     &c.meter,
			direction: TrafficToLocal,
		}, stream)
		results <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(&trafficWriter{
			Writer:    stream,
			meter:     &c.meter,
			direction: TrafficFromLocal,
		}, local)
		results <- struct{}{}
	}()

	<-results
	_ = local.Close()
	_ = stream.Close()
	<-results
}

type trafficWriter struct {
	io.Writer
	meter     *trafficMeter
	direction TrafficDirection
}

func (w *trafficWriter) Write(buffer []byte) (int, error) {
	written, err := w.Writer.Write(buffer)
	w.meter.add(w.direction, int64(written))
	return written, err
}

func buildTunnelURL(rawURL, tunnelPath, domain string, publicPort uint16) (string, error) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
	}
	if parsed.Host == "" {
		return "", errors.New("server URL must include a host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported server URL scheme %q", parsed.Scheme)
	}
	if err := validateTunnelPath(tunnelPath); err != nil {
		return "", err
	}
	parsed.Path = tunnelPath
	parsed.RawPath = ""
	parsed.Fragment = ""
	query := parsed.Query()
	query.Set("port", strconv.Itoa(int(publicPort)))
	if domain != "" {
		query.Set("domain", domain)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func classifyHandshakeError(response *http.Response, dialErr error) error {
	if response == nil {
		return dialErr
	}

	var kind error
	switch response.Header.Get(ErrorCodeHeader) {
	case "authentication_rejected":
		kind = ErrAuthenticationRejected
	case "verification_unavailable":
		kind = ErrVerificationUnavailable
	case "rate_limited":
		kind = ErrRateLimited
	case "tunnel_busy":
		kind = ErrTunnelBusy
	case "public_port_in_use":
		kind = ErrPublicPortInUse
	case "public_port_permission_denied":
		kind = ErrPublicPortPermissionDenied
	case "public_bind_failed":
		kind = ErrPublicBindFailed
	case "server_closed":
		kind = ErrServerClosed
	default:
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = ErrAuthenticationRejected
		case http.StatusTooManyRequests:
			kind = ErrRateLimited
		case http.StatusConflict:
			kind = ErrTunnelBusy
		}
	}

	wrapped := dialErr
	if kind != nil {
		wrapped = fmt.Errorf("%w: %v", kind, dialErr)
	}
	return &HandshakeError{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		Err:        wrapped,
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
