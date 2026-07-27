package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyHandler forwards HTTP, streaming responses, and websocket upgrades
// through a fresh yamux stream for every incoming request.
func (s *Server) ProxyHandler() http.Handler {
	return http.HandlerFunc(s.serveProxy)
}

func (s *Server) serveProxy(writer http.ResponseWriter, request *http.Request) {
	session, err := s.currentSession()
	if err != nil {
		writer.Header().Set("Retry-After", "1")
		http.Error(writer, "reverse tunnel is offline", http.StatusServiceUnavailable)
		return
	}

	requestID := fmt.Sprintf("%016x", s.requestCounter.Add(1))
	startedAt := time.Now().UTC()
	remoteAddr := effectiveRemoteAddr(request)
	originalProto := effectiveProto(request)
	discardUntrustedForwardingHeaders(request)
	request.Header.Set("X-Reverse-Request-ID", requestID)

	var requestBytes atomic.Int64
	if request.Body != nil {
		request.Body = &countingReadCloser{
			ReadCloser: request.Body,
			count:      &requestBytes,
		}
	}

	recorder := &trackingResponseWriter{ResponseWriter: writer}
	var proxyError errorState
	started := Event{
		Type:       EventRequestStarted,
		ID:         requestID,
		Time:       startedAt,
		Method:     request.Method,
		Host:       request.Host,
		Path:       request.URL.RequestURI(),
		RemoteAddr: remoteAddr,
	}
	s.emitEvent(session, started)

	transport := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
		MaxIdleConns:      0,
		MaxConnsPerHost:   1,
		IdleConnTimeout:   time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return session.openProxyStream(ctx)
		},
	}
	defer transport.CloseIdleConnections()

	originalHost := request.Host
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			outgoing := proxyRequest.Out
			outgoing.URL.Scheme = "http"
			outgoing.URL.Host = "reverse.internal"
			outgoing.Host = originalHost

			// ReverseProxy removes hop-by-hop and inbound forwarding headers
			// before Rewrite runs. Rebuild one canonical, trusted hop here so
			// a Connection header cannot remove or spoof forwarding metadata.
			outgoing.Header.Del("Forwarded")
			outgoing.Header.Del("X-Forwarded-For")
			outgoing.Header.Del("X-Forwarded-Host")
			outgoing.Header.Del("X-Forwarded-Proto")
			outgoing.Header.Del("X-Real-IP")
			if remoteIP := hostOnly(remoteAddr); remoteIP != "" {
				outgoing.Header.Set("X-Forwarded-For", remoteIP)
				outgoing.Header.Set("X-Real-IP", remoteIP)
			}
			outgoing.Header.Set("X-Forwarded-Host", originalHost)
			outgoing.Header.Set("X-Forwarded-Proto", originalProto)
		},
		ModifyResponse: func(response *http.Response) error {
			recorder.observeStatus(response.StatusCode)
			return nil
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, proxyErr error) {
			proxyError.set(proxyErr)
			if !recorder.wroteHeader() {
				http.Error(response, "reverse tunnel upstream error", http.StatusBadGateway)
			}
		},
	}
	proxy.ServeHTTP(recorder, request)

	finishedAt := time.Now().UTC()
	status := recorder.statusCode()
	if status == 0 {
		status = http.StatusOK
	}
	if proxyErr := proxyError.get(); proxyErr != nil {
		s.emitEvent(session, Event{
			Type:       EventRequestError,
			ID:         requestID,
			Time:       finishedAt,
			Method:     request.Method,
			Host:       request.Host,
			Path:       request.URL.RequestURI(),
			RemoteAddr: remoteAddr,
			Status:     status,
			Error:      proxyErr.Error(),
		})
	}
	s.emitEvent(session, Event{
		Type:       EventRequestFinished,
		ID:         requestID,
		Time:       finishedAt,
		Method:     request.Method,
		Host:       request.Host,
		Path:       request.URL.RequestURI(),
		RemoteAddr: remoteAddr,
		Status:     status,
		Duration:   finishedAt.Sub(startedAt).Milliseconds(),
		BytesIn:    requestBytes.Load(),
		BytesOut:   recorder.bytesWritten(),
	})
}

type meteredConn struct {
	net.Conn
	meter       *trafficMeter
	release     func()
	releaseOnce sync.Once
}

func (c *meteredConn) Read(buffer []byte) (int, error) {
	read, err := c.Conn.Read(buffer)
	c.meter.add(TrafficFromLocal, int64(read))
	return read, err
}

func (c *meteredConn) Write(buffer []byte) (int, error) {
	written, err := c.Conn.Write(buffer)
	c.meter.add(TrafficToLocal, int64(written))
	return written, err
}

func (c *meteredConn) Close() error {
	err := c.Conn.Close()
	if c.release != nil {
		c.releaseOnce.Do(c.release)
	}
	return err
}

func (s *serverSession) openProxyStream(ctx context.Context) (net.Conn, error) {
	var sessionClosed <-chan struct{}
	if s.mux != nil {
		sessionClosed = s.mux.CloseChan()
	}
	select {
	case s.streamSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-sessionClosed:
		return nil, ErrTunnelDisconnected
	}

	release := func() {
		<-s.streamSlots
	}
	if s.mux == nil {
		release()
		return nil, ErrTunnelDisconnected
	}
	stream, err := s.mux.Open()
	if err != nil {
		release()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = stream.Close()
		release()
		return nil, err
	}
	return &meteredConn{
		Conn:    stream,
		meter:   &s.meter,
		release: release,
	}, nil
}

type countingReadCloser struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	read, err := r.ReadCloser.Read(buffer)
	r.count.Add(int64(read))
	return read, err
}

type trackingResponseWriter struct {
	http.ResponseWriter
	mu     sync.Mutex
	status int
	bytes  int64
	wrote  bool
}

func (w *trackingResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// Informational responses do not commit the final response status.
		// In particular, a 103 Early Hints response may be followed by 200.
		w.ResponseWriter.WriteHeader(status)
		return
	}

	w.mu.Lock()
	if w.wrote {
		w.mu.Unlock()
		return
	}
	w.status = status
	w.wrote = true
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingResponseWriter) Write(buffer []byte) (int, error) {
	w.mu.Lock()
	if !w.wrote {
		w.wrote = true
		if w.status == 0 {
			w.status = http.StatusOK
		}
	}
	w.mu.Unlock()

	written, err := w.ResponseWriter.Write(buffer)
	w.mu.Lock()
	w.bytes += int64(written)
	w.mu.Unlock()
	return written, err
}

func (w *trackingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *trackingResponseWriter) Flush() {
	if !w.wroteHeader() {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *trackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *trackingResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (w *trackingResponseWriter) wroteHeader() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wrote
}

func (w *trackingResponseWriter) statusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *trackingResponseWriter) bytesWritten() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

func (w *trackingResponseWriter) observeStatus(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
}

type errorState struct {
	mu  sync.Mutex
	err error
}

func (s *errorState) set(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *errorState) get() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
