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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const (
	defaultControlTimeout    = 10 * time.Second
	defaultReadHeaderTimeout = 10 * time.Second
	defaultEventBuffer       = 1024
	defaultMaxProxyStreams   = 128
	maxProxyStreams          = 256
)

// ServerConfig configures the websocket endpoint and the per-session public
// HTTP listener. PublicBindHost defaults to loopback, which is appropriate when
// Caddy is the public entry point.
type ServerConfig struct {
	Verify                  VerifyFunc
	TunnelPath              string
	PublicBindHost          string
	OriginPatterns          []string
	ControlTimeout          time.Duration
	PublicReadHeaderTimeout time.Duration
	EventBuffer             int

	// MaxProxyStreams bounds simultaneous HTTP and upgraded connections using
	// one tunnel session.
	MaxProxyStreams int

	// VerifyConcurrency bounds verifier work process-wide. Authentication
	// failures from one source are delayed exponentially between these bounds.
	VerifyConcurrency       int
	VerifyFailureBackoff    time.Duration
	VerifyFailureMaxBackoff time.Duration
	OnEvent                 EventCallback
	OnStatus                StatusCallback
	OnTraffic               TrafficCallback
}

// Server owns at most one active tunnel session.
type Server struct {
	config ServerConfig

	mu      sync.RWMutex
	active  *serverSession
	pending bool
	closed  bool
	done    chan struct{}

	requestCounter atomic.Uint64
	verifications  *verificationLimiter
}

func NewServer(config ServerConfig) (*Server, error) {
	if config.Verify == nil {
		return nil, errors.New("tunnel verifier is required")
	}
	if config.TunnelPath == "" {
		config.TunnelPath = DefaultTunnelPath
	}
	if err := validateTunnelPath(config.TunnelPath); err != nil {
		return nil, err
	}
	if config.PublicBindHost == "" {
		config.PublicBindHost = "127.0.0.1"
	}
	if config.ControlTimeout <= 0 {
		config.ControlTimeout = defaultControlTimeout
	}
	if config.PublicReadHeaderTimeout <= 0 {
		config.PublicReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if config.EventBuffer <= 0 {
		config.EventBuffer = defaultEventBuffer
	}
	if config.MaxProxyStreams <= 0 {
		config.MaxProxyStreams = defaultMaxProxyStreams
	}
	if config.MaxProxyStreams > maxProxyStreams {
		return nil, fmt.Errorf("max proxy streams cannot exceed %d", maxProxyStreams)
	}
	if config.VerifyConcurrency <= 0 {
		config.VerifyConcurrency = defaultVerifyConcurrency
	}
	if config.VerifyConcurrency > maxVerifyConcurrency {
		return nil, fmt.Errorf("verification concurrency cannot exceed %d", maxVerifyConcurrency)
	}
	if config.VerifyFailureBackoff <= 0 {
		config.VerifyFailureBackoff = defaultVerifyBackoff
	}
	if config.VerifyFailureMaxBackoff <= 0 {
		config.VerifyFailureMaxBackoff = defaultVerifyMaxBackoff
		if config.VerifyFailureMaxBackoff < config.VerifyFailureBackoff {
			config.VerifyFailureMaxBackoff = config.VerifyFailureBackoff
		}
	}
	if config.VerifyFailureMaxBackoff < config.VerifyFailureBackoff {
		return nil, errors.New("verification max backoff cannot be less than initial backoff")
	}

	return &Server{
		config:        config,
		done:          make(chan struct{}),
		verifications: newVerificationLimiter(config.VerifyConcurrency, config.VerifyFailureBackoff, config.VerifyFailureMaxBackoff),
	}, nil
}

// Handler serves both the tunnel endpoint and the active public reverse proxy.
// It is useful when a single internal HTTP server sits behind Caddy.
func (s *Server) Handler() http.Handler {
	tunnel := s.TunnelHandler()
	proxy := s.ProxyHandler()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == s.config.TunnelPath {
			tunnel.ServeHTTP(writer, request)
			return
		}
		proxy.ServeHTTP(writer, request)
	})
}

func (s *Server) TunnelHandler() http.Handler {
	return http.HandlerFunc(s.serveTunnel)
}

// ActiveSession returns a stable snapshot of the current session.
func (s *Server) ActiveSession() (SessionInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.active == nil {
		return SessionInfo{}, false
	}
	return s.active.info, true
}

// Health returns a point-in-time process and tunnel readiness snapshot.
func (s *Server) Health() Health {
	s.mu.RLock()
	defer s.mu.RUnlock()

	health := Health{
		Healthy: !s.closed,
		State:   StatusOffline,
	}
	if s.closed {
		health.State = StatusStopped
	}
	if !s.closed && s.active != nil {
		info := s.active.info
		health.State = StatusOnline
		health.Session = &info
	}
	return health
}

func (s *Server) HealthHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		health := s.Health()
		writer.Header().Set("Content-Type", "application/json")
		if !health.Healthy {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(writer).Encode(health)
	})
}

// Close stops the active tunnel and its public listener. The context bounds
// graceful shutdown of public HTTP requests.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	active := s.active
	if active == nil {
		close(s.done)
	}
	s.mu.Unlock()

	if active == nil {
		return nil
	}
	err := active.shutdown(ctx)
	close(s.done)
	return err
}

func (s *Server) serveTunnel(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "tunnel endpoint requires GET", http.StatusMethodNotAllowed)
		return
	}
	if s.isClosed() {
		writer.Header().Set(ErrorCodeHeader, "server_closed")
		http.Error(writer, ErrServerClosed.Error(), http.StatusServiceUnavailable)
		return
	}

	publicPort, err := parsePublicPort(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	domain := request.URL.Query().Get("domain")
	if domain == "" {
		domain = request.Host
		if host, _, splitErr := net.SplitHostPort(domain); splitErr == nil {
			domain = host
		}
	}

	remoteAddr := effectiveRemoteAddr(request)
	verifyRequest := VerifyRequest{
		Token:      requestToken(request),
		Domain:     domain,
		RemoteAddr: remoteAddr,
		PublicPort: publicPort,
		Header:     request.Header.Clone(),
	}
	verifyKey := hostOnly(remoteAddr)
	if verifyKey == "" {
		verifyKey = "unknown"
	}
	if retryAfter := s.verifications.retryAfter(verifyKey, time.Now()); retryAfter > 0 {
		writeRateLimited(writer, retryAfter)
		return
	}
	if !s.verifications.tryAcquire() {
		writeRateLimited(writer, s.config.VerifyFailureBackoff)
		return
	}
	var verifyErr error
	func() {
		defer s.verifications.release()
		verifyErr = s.config.Verify(request.Context(), verifyRequest)
	}()
	if verifyErr != nil {
		if !errors.Is(verifyErr, ErrAuthenticationRejected) {
			writer.Header().Set(ErrorCodeHeader, "verification_unavailable")
			http.Error(writer, "authentication service unavailable", http.StatusServiceUnavailable)
			return
		}
		s.verifications.recordFailure(verifyKey, time.Now())
		writer.Header().Set(ErrorCodeHeader, "authentication_rejected")
		http.Error(writer, "authentication failed", http.StatusUnauthorized)
		return
	}
	s.verifications.clear(verifyKey)

	if err := s.beginHandshake(); err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrServerClosed) {
			status = http.StatusServiceUnavailable
			writer.Header().Set(ErrorCodeHeader, "server_closed")
		} else {
			writer.Header().Set(ErrorCodeHeader, "tunnel_busy")
		}
		http.Error(writer, err.Error(), status)
		return
	}
	defer s.endHandshake()

	listenAddress := net.JoinHostPort(s.config.PublicBindHost, strconv.Itoa(int(publicPort)))
	publicListener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		switch classifyListenError(err) {
		case ErrPublicPortInUse:
			portErr := &PortInUseError{Address: listenAddress, Err: err}
			writer.Header().Set(ErrorCodeHeader, "public_port_in_use")
			http.Error(writer, portErr.Error(), http.StatusConflict)
		case ErrPublicPortPermissionDenied:
			writer.Header().Set(ErrorCodeHeader, "public_port_permission_denied")
			http.Error(writer, ErrPublicPortPermissionDenied.Error(), http.StatusForbidden)
		default:
			writer.Header().Set(ErrorCodeHeader, "public_bind_failed")
			http.Error(writer, "could not bind public listener", http.StatusServiceUnavailable)
		}
		return
	}
	listenerOwned := false
	defer func() {
		if !listenerOwned {
			_ = publicListener.Close()
		}
	}()

	socket, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		OriginPatterns: s.config.OriginPatterns,
	})
	if err != nil {
		return
	}

	socketConn := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
	muxConfig := yamux.DefaultConfig()
	muxConfig.LogOutput = io.Discard
	muxSession, err := yamux.Server(socketConn, muxConfig)
	if err != nil {
		_ = socket.Close(websocket.StatusInternalError, "could not start multiplexed session")
		_ = socketConn.Close()
		return
	}

	controlConn, err := s.acceptControl(request.Context(), muxSession)
	if err != nil {
		_ = muxSession.Close()
		_ = socketConn.Close()
		return
	}

	session := &serverSession{
		server: s,
		info: SessionInfo{
			Domain:      domain,
			RemoteAddr:  effectiveRemoteAddr(request),
			PublicPort:  publicPort,
			ConnectedAt: time.Now().UTC(),
		},
		mux:         muxSession,
		socket:      socketConn,
		listener:    publicListener,
		control:     newControlWriter(controlConn, s.config.EventBuffer),
		streamSlots: make(chan struct{}, s.config.MaxProxyStreams),
	}
	session.meter.callback = s.config.OnTraffic
	session.publicServer = &http.Server{
		Handler:           s.ProxyHandler(),
		ReadHeaderTimeout: s.config.PublicReadHeaderTimeout,
	}

	if err := s.activate(session); err != nil {
		session.close()
		return
	}
	listenerOwned = true
	defer func() {
		session.close()
		s.deactivate(session)
	}()

	go session.servePublic()

	online := Event{
		Type:       EventSessionOnline,
		Time:       time.Now().UTC(),
		PublicPort: publicPort,
	}
	s.emitEvent(session, online)
	callStatus(s.config.OnStatus, Status{
		State:      StatusOnline,
		At:         online.Time,
		PublicPort: publicPort,
	})

	select {
	case <-muxSession.CloseChan():
	case <-request.Context().Done():
	case <-s.done:
	}

	offline := Event{
		Type:       EventSessionOffline,
		Time:       time.Now().UTC(),
		PublicPort: publicPort,
	}
	s.emitEvent(session, offline)
	callStatus(s.config.OnStatus, Status{
		State:      StatusOffline,
		At:         offline.Time,
		PublicPort: publicPort,
	})
}

func (s *Server) beginHandshake() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServerClosed
	}
	if s.active != nil || s.pending {
		return ErrSessionActive
	}
	s.pending = true
	return nil
}

func (s *Server) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func (s *Server) endHandshake() {
	s.mu.Lock()
	s.pending = false
	s.mu.Unlock()
}

func (s *Server) activate(session *serverSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServerClosed
	}
	if s.active != nil {
		return ErrSessionActive
	}
	s.active = session
	return nil
}

func (s *Server) deactivate(session *serverSession) {
	s.mu.Lock()
	if s.active == session {
		s.active = nil
	}
	s.mu.Unlock()
}

func (s *Server) currentSession() (*serverSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrServerClosed
	}
	if s.active == nil {
		return nil, ErrNoActiveSession
	}
	return s.active, nil
}

func (s *Server) acceptControl(ctx context.Context, session *yamux.Session) (net.Conn, error) {
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan acceptResult, 1)
	go func() {
		conn, err := session.Accept()
		result <- acceptResult{conn: conn, err: err}
	}()

	timer := time.NewTimer(s.config.ControlTimeout)
	defer timer.Stop()
	select {
	case accepted := <-result:
		return accepted.conn, accepted.err
	case <-timer.C:
		return nil, errors.New("timed out waiting for control stream")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrServerClosed
	}
}

func (s *Server) emitEvent(session *serverSession, event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	// The client dashboard only consumes terminal request events. Keeping
	// request-start noise out of the bounded control queue leaves room for
	// completion/error records while server-side callbacks still see both.
	if event.Type != EventRequestStarted {
		session.control.emit(event)
	}
	callEvent(s.config.OnEvent, event)
}

func parsePublicPort(request *http.Request) (uint16, error) {
	value := request.URL.Query().Get("port")
	if value == "" {
		value = request.URL.Query().Get("public_port")
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("query parameter \"port\" must be between 1 and 65535")
	}
	return uint16(port), nil
}

func requestToken(request *http.Request) string {
	encoded := request.Header.Get(TokenHeader)
	if strings.HasPrefix(encoded, "v1.") {
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, "v1."))
		if err == nil {
			return string(decoded)
		}
		return ""
	}

	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	const bearer = "Bearer "
	if len(authorization) >= len(bearer) && strings.EqualFold(authorization[:len(bearer)], bearer) {
		return strings.TrimSpace(authorization[len(bearer):])
	}
	return encoded
}

func writeRateLimited(writer http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(retryAfter / time.Second)
	if retryAfter%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	writer.Header().Set(ErrorCodeHeader, "rate_limited")
	writer.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(writer, "too many authentication attempts", http.StatusTooManyRequests)
}

func classifyListenError(err error) error {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return ErrPublicPortInUse
	case errors.Is(err, syscall.EACCES):
		return ErrPublicPortPermissionDenied
	default:
		return ErrPublicBindFailed
	}
}

type serverSession struct {
	server *Server
	info   SessionInfo

	mux          *yamux.Session
	socket       net.Conn
	listener     net.Listener
	control      *controlWriter
	publicServer *http.Server
	meter        trafficMeter
	streamSlots  chan struct{}
	closeOnce    sync.Once
}

func (s *serverSession) servePublic() {
	err := s.publicServer.Serve(s.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		s.server.emitEvent(s, Event{
			Type:  EventTunnelError,
			Time:  time.Now().UTC(),
			Error: fmt.Sprintf("public listener failed: %v", err),
		})
		s.close()
	}
}

func (s *serverSession) shutdown(ctx context.Context) error {
	var shutdownErr error
	if s.publicServer != nil {
		shutdownErr = s.publicServer.Shutdown(ctx)
	}
	s.close()
	return shutdownErr
}

func (s *serverSession) close() {
	s.closeOnce.Do(func() {
		if s.publicServer != nil {
			_ = s.publicServer.Close()
		}
		if s.control != nil {
			s.control.close()
		}
		if s.mux != nil {
			_ = s.mux.Close()
		}
		if s.socket != nil {
			_ = s.socket.Close()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}
