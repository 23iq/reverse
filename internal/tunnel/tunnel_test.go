package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTrackingResponseWriterAllowsEarlyHintsBeforeFinalStatus(t *testing.T) {
	t.Parallel()
	underlying := &statusResponseWriter{header: make(http.Header)}
	tracked := &trackingResponseWriter{ResponseWriter: underlying}

	tracked.WriteHeader(http.StatusEarlyHints)
	tracked.WriteHeader(http.StatusOK)
	_, _ = tracked.Write([]byte("body"))

	if got, want := fmt.Sprint(underlying.statuses), "[103 200]"; got != want {
		t.Fatalf("forwarded statuses = %s, want %s", got, want)
	}
	if got := tracked.statusCode(); got != http.StatusOK {
		t.Fatalf("tracked status = %d, want 200", got)
	}
	if got := tracked.bytesWritten(); got != 4 {
		t.Fatalf("tracked bytes = %d, want 4", got)
	}
}

func TestTunnelPathMustBeCleanNonRootAndLiteral(t *testing.T) {
	t.Parallel()

	for _, tunnelPath := range []string{
		"/",
		"relative",
		"/_reverse/tunnel/",
		"/_reverse/../tunnel",
		"//_reverse/tunnel",
		"/_reverse/{tunnel}",
		"/_reverse/tunnel?debug=true",
		"/_reverse%2Ftunnel",
	} {
		t.Run(tunnelPath, func(t *testing.T) {
			_, err := NewServer(ServerConfig{
				Verify:     func(context.Context, VerifyRequest) error { return nil },
				TunnelPath: tunnelPath,
			})
			if err == nil {
				t.Fatalf("NewServer accepted unsafe tunnel path %q", tunnelPath)
			}
		})
	}

	server, err := NewServer(ServerConfig{
		Verify:     func(context.Context, VerifyRequest) error { return nil },
		TunnelPath: "/control/tunnel",
	})
	if err != nil {
		t.Fatalf("NewServer(valid path): %v", err)
	}
	exact := httptest.NewRecorder()
	server.Handler().ServeHTTP(exact, httptest.NewRequest(http.MethodPost, "http://example.test/control/tunnel", nil))
	if exact.Code != http.StatusMethodNotAllowed {
		t.Fatalf("exact tunnel path status = %d, want 405", exact.Code)
	}
	trailing := httptest.NewRecorder()
	server.Handler().ServeHTTP(trailing, httptest.NewRequest(http.MethodPost, "http://example.test/control/tunnel/", nil))
	if trailing.Code != http.StatusServiceUnavailable {
		t.Fatalf("trailing path status = %d, want proxy's 503", trailing.Code)
	}
}

func TestVerificationErrorsAndRateLimitsAreTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		verifyErr  error
		statusCode int
		errorCode  string
		kind       error
		retryable  bool
	}{
		{
			name:       "authentication rejection",
			verifyErr:  fmt.Errorf("password mismatch: %w", ErrAuthenticationRejected),
			statusCode: http.StatusUnauthorized,
			errorCode:  "authentication_rejected",
			kind:       ErrAuthenticationRejected,
			retryable:  false,
		},
		{
			name:       "internal verifier failure",
			verifyErr:  errors.New("credential store unavailable"),
			statusCode: http.StatusServiceUnavailable,
			errorCode:  "verification_unavailable",
			kind:       ErrVerificationUnavailable,
			retryable:  true,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(ServerConfig{
				Verify: func(context.Context, VerifyRequest) error {
					return test.verifyErr
				},
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://example.test/_reverse/tunnel?port=3000", nil)
			request.RemoteAddr = fmt.Sprintf("198.51.100.%d:4321", index+1)
			recorder := httptest.NewRecorder()
			server.TunnelHandler().ServeHTTP(recorder, request)
			response := recorder.Result()
			defer response.Body.Close()

			if response.StatusCode != test.statusCode {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.statusCode)
			}
			if got := response.Header.Get(ErrorCodeHeader); got != test.errorCode {
				t.Fatalf("%s = %q, want %q", ErrorCodeHeader, got, test.errorCode)
			}
			classified := classifyHandshakeError(response, errors.New("websocket rejected"))
			if !errors.Is(classified, test.kind) {
				t.Fatalf("classified error = %v, want errors.Is(%v)", classified, test.kind)
			}
			var handshakeErr *HandshakeError
			if !errors.As(classified, &handshakeErr) {
				t.Fatalf("classified error = %T, want *HandshakeError", classified)
			}
			if handshakeErr.Retryable() != test.retryable {
				t.Fatalf("Retryable() = %t, want %t", handshakeErr.Retryable(), test.retryable)
			}
		})
	}
}

func TestVerificationLimiterBoundsConcurrencyAndBacksOffFailures(t *testing.T) {
	t.Parallel()

	t.Run("global concurrency", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var enteredOnce sync.Once
		var releaseOnce sync.Once
		releaseVerifier := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseVerifier()

		server, err := NewServer(ServerConfig{
			VerifyConcurrency:    1,
			VerifyFailureBackoff: time.Minute,
			Verify: func(context.Context, VerifyRequest) error {
				enteredOnce.Do(func() { close(entered) })
				<-release
				return ErrAuthenticationRejected
			},
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}

		firstDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			request := httptest.NewRequest(http.MethodGet, "http://example.test/_reverse/tunnel?port=3000", nil)
			request.RemoteAddr = "198.51.100.10:1111"
			recorder := httptest.NewRecorder()
			server.TunnelHandler().ServeHTTP(recorder, request)
			firstDone <- recorder
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("first verifier did not start")
		}

		secondRequest := httptest.NewRequest(http.MethodGet, "http://example.test/_reverse/tunnel?port=3001", nil)
		secondRequest.RemoteAddr = "198.51.100.11:2222"
		second := httptest.NewRecorder()
		server.TunnelHandler().ServeHTTP(second, secondRequest)
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("concurrent verification status = %d, want 429", second.Code)
		}
		if second.Header().Get(ErrorCodeHeader) != "rate_limited" {
			t.Fatalf("concurrent verification error code = %q", second.Header().Get(ErrorCodeHeader))
		}

		releaseVerifier()
		select {
		case first := <-firstDone:
			if first.Code != http.StatusUnauthorized {
				t.Fatalf("first verification status = %d, want 401", first.Code)
			}
		case <-time.After(time.Second):
			t.Fatal("first verifier did not finish")
		}
	})

	t.Run("per-IP failure backoff", func(t *testing.T) {
		var calls atomic.Int32
		server, err := NewServer(ServerConfig{
			VerifyFailureBackoff: time.Minute,
			Verify: func(context.Context, VerifyRequest) error {
				calls.Add(1)
				return ErrAuthenticationRejected
			},
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}

		send := func() *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodGet, "http://example.test/_reverse/tunnel?port=3000", nil)
			request.RemoteAddr = "203.0.113.44:5555"
			recorder := httptest.NewRecorder()
			server.TunnelHandler().ServeHTTP(recorder, request)
			return recorder
		}
		if first := send(); first.Code != http.StatusUnauthorized {
			t.Fatalf("first failure status = %d, want 401", first.Code)
		}
		second := send()
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("backoff status = %d, want 429", second.Code)
		}
		if calls.Load() != 1 {
			t.Fatalf("verifier calls = %d, want 1", calls.Load())
		}
		response := second.Result()
		defer response.Body.Close()
		classified := classifyHandshakeError(response, errors.New("websocket rejected"))
		if !errors.Is(classified, ErrRateLimited) {
			t.Fatalf("classified backoff error = %v, want ErrRateLimited", classified)
		}
		var handshakeErr *HandshakeError
		if !errors.As(classified, &handshakeErr) || !handshakeErr.Retryable() {
			t.Fatalf("rate limit should be a retryable HandshakeError: %v", classified)
		}
	})
}

func TestListenErrorsAreClassified(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		want error
	}{
		{err: &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}, want: ErrPublicPortInUse},
		{err: &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EACCES}, want: ErrPublicPortPermissionDenied},
		{err: &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRNOTAVAIL}, want: ErrPublicBindFailed},
	}
	for _, test := range tests {
		if got := classifyListenError(test.err); !errors.Is(got, test.want) {
			t.Errorf("classifyListenError(%v) = %v, want %v", test.err, got, test.want)
		}
	}

	for errorCode, want := range map[string]error{
		"public_port_in_use":            ErrPublicPortInUse,
		"public_port_permission_denied": ErrPublicPortPermissionDenied,
		"public_bind_failed":            ErrPublicBindFailed,
	} {
		response := &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     make(http.Header),
		}
		response.Header.Set(ErrorCodeHeader, errorCode)
		classified := classifyHandshakeError(response, errors.New("websocket rejected"))
		if !errors.Is(classified, want) {
			t.Errorf("error code %q classified as %v, want %v", errorCode, classified, want)
		}
	}
}

func TestProxyStreamSlotWaitRespectsContext(t *testing.T) {
	t.Parallel()

	session := &serverSession{streamSlots: make(chan struct{}, 1)}
	session.streamSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	_, err := session.openProxyStream(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("openProxyStream error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled stream-slot wait took %s", elapsed)
	}
}

func TestServerCloseDrainsDirectHTTPRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRequest()

	server, err := NewServer(ServerConfig{
		Verify: func(context.Context, VerifyRequest) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	session := &serverSession{
		server:      server,
		listener:    listener,
		streamSlots: make(chan struct{}, 1),
		publicServer: &http.Server{
			Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				_, _ = io.WriteString(writer, "drained")
			}),
		},
	}
	server.mu.Lock()
	server.active = session
	server.mu.Unlock()
	go session.servePublic()

	client := &http.Client{Timeout: 2 * time.Second}
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := client.Get("http://" + listener.Addr().String())
		if requestErr != nil {
			requestDone <- requestErr
			return
		}
		defer response.Body.Close()
		body, requestErr := io.ReadAll(response.Body)
		if requestErr == nil && string(body) != "drained" {
			requestErr = fmt.Errorf("response body = %q, want %q", body, "drained")
		}
		requestDone <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("direct request did not start")
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClose()
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- server.Close(closeCtx)
	}()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before request drained: %v", err)
	case <-server.done:
		t.Fatal("server done closed before request drained")
	case <-time.After(50 * time.Millisecond):
	}

	releaseRequest()
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("direct request failed during drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct request did not complete")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after request drained")
	}
	select {
	case <-server.done:
	default:
		t.Fatal("server done remained open after shutdown")
	}
}

type statusResponseWriter struct {
	header   http.Header
	statuses []int
	body     bytes.Buffer
}

func (w *statusResponseWriter) Header() http.Header {
	return w.header
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func TestTunnelHTTPStreamingAndWebSocket(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/echo":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read local request: %v", err)
				return
			}
			writer.Header().Set("X-Local-Service", "true")
			_, _ = fmt.Fprintf(
				writer,
				"%s %s host=%s proto=%s forwarded=%s body=%s",
				request.Method,
				request.URL.Path,
				request.Host,
				request.Header.Get("X-Forwarded-Proto"),
				request.Header.Get("X-Forwarded-For"),
				body,
			)
		case "/stream":
			flusher, ok := writer.(http.Flusher)
			if !ok {
				t.Error("local response writer does not support flush")
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: first\n\n")
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(writer, "data: second\n\n")
		case "/socket":
			socket, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer socket.Close(websocket.StatusNormalClosure, "test complete")
			messageType, message, err := socket.Read(request.Context())
			if err != nil {
				return
			}
			_ = socket.Write(request.Context(), messageType, append([]byte("echo:"), message...))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer local.Close()

	server, err := NewServer(ServerConfig{
		Verify: func(_ context.Context, request VerifyRequest) error {
			if request.Token != "correct horse" {
				return ErrAuthenticationRejected
			}
			return nil
		},
		PublicBindHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close(context.Background())

	tunnelHTTP := httptest.NewServer(server.TunnelHandler())
	defer tunnelHTTP.Close()

	publicPort := freePort(t)
	statuses := make(chan Status, 16)
	events := make(chan Event, 64)
	var trafficBytes atomic.Int64
	client, err := NewClient(ClientConfig{
		ServerURL:    tunnelHTTP.URL,
		Token:        "correct horse",
		Domain:       "tunnel.example.test",
		PublicPort:   publicPort,
		LocalAddress: strings.TrimPrefix(local.URL, "http://"),
		OnStatus: func(status Status) {
			statuses <- status
		},
		OnEvent: func(event Event) {
			events <- event
		},
		OnTraffic: func(traffic Traffic) {
			trafficBytes.Add(traffic.Bytes)
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- client.RunOnce(ctx)
	}()
	waitForStatus(t, statuses, StatusOnline)

	health := server.Health()
	if !health.Healthy || health.State != StatusOnline || health.Session == nil {
		t.Fatalf("unexpected online health: %+v", health)
	}
	if health.Session.Domain != "tunnel.example.test" || health.Session.PublicPort != publicPort {
		t.Fatalf("unexpected session health: %+v", health.Session)
	}

	publicURL := "http://127.0.0.1:" + strconv.Itoa(int(publicPort))
	request, err := http.NewRequest(http.MethodPost, publicURL+"/echo?hello=world", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Host = "tunnel.example.test"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Connection", "X-Forwarded-Proto")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("proxied HTTP request: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read proxied response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response status: %s (%s)", response.Status, body)
	}
	if response.Header.Get("X-Local-Service") != "true" {
		t.Fatalf("local response header was not proxied: %v", response.Header)
	}
	gotBody := string(body)
	for _, expected := range []string{
		"POST /echo",
		"host=tunnel.example.test",
		"proto=https",
		"forwarded=203.0.113.9",
		"body=payload",
	} {
		if !strings.Contains(gotBody, expected) {
			t.Fatalf("proxied response %q does not contain %q", gotBody, expected)
		}
	}

	streamResponse, err := http.Get(publicURL + "/stream")
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	streamBody, err := io.ReadAll(streamResponse.Body)
	_ = streamResponse.Body.Close()
	if err != nil {
		t.Fatalf("read streaming response: %v", err)
	}
	if string(streamBody) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("unexpected streaming body: %q", streamBody)
	}

	socket, _, err := websocket.Dial(context.Background(), "ws://127.0.0.1:"+strconv.Itoa(int(publicPort))+"/socket", nil)
	if err != nil {
		t.Fatalf("websocket through reverse proxy: %v", err)
	}
	if err := socket.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
	_, message, err := socket.Read(context.Background())
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if string(message) != "echo:hello" {
		t.Fatalf("unexpected websocket reply: %q", message)
	}
	_ = socket.Close(websocket.StatusNormalClosure, "test complete")

	waitForEvent(t, events, EventRequestFinished, "/echo?hello=world")
	if trafficBytes.Load() == 0 {
		t.Fatal("traffic callback did not report bytes")
	}

	secondPort := freePort(t)
	secondClient, err := NewClient(ClientConfig{
		ServerURL:    tunnelHTTP.URL,
		Token:        "correct horse",
		PublicPort:   secondPort,
		LocalAddress: strings.TrimPrefix(local.URL, "http://"),
	})
	if err != nil {
		t.Fatalf("NewClient(second): %v", err)
	}
	err = secondClient.RunOnce(context.Background())
	if !errors.Is(err, ErrTunnelBusy) {
		t.Fatalf("second client error = %v, want ErrTunnelBusy", err)
	}

	stillOnline, err := http.Get(publicURL + "/echo")
	if err != nil {
		t.Fatalf("active tunnel was disrupted by busy rejection: %v", err)
	}
	_ = stillOnline.Body.Close()
	if stillOnline.StatusCode != http.StatusOK {
		t.Fatalf("active tunnel status after rejection = %s", stillOnline.Status)
	}

	cancel()
	select {
	case err := <-runResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunOnce did not stop after cancellation")
	}
}

func TestHandshakeRejectionsAreTypedAndNonDestructive(t *testing.T) {
	t.Run("authentication happens before bind", func(t *testing.T) {
		server, err := NewServer(ServerConfig{
			Verify: func(context.Context, VerifyRequest) error {
				return fmt.Errorf("bad password: %w", ErrAuthenticationRejected)
			},
			PublicBindHost: "127.0.0.1",
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		defer server.Close(context.Background())
		tunnelHTTP := httptest.NewServer(server.TunnelHandler())
		defer tunnelHTTP.Close()

		port := freePort(t)
		client, err := NewClient(ClientConfig{
			ServerURL:    tunnelHTTP.URL,
			Token:        "wrong",
			PublicPort:   port,
			LocalAddress: "127.0.0.1:1",
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		err = client.RunOnce(context.Background())
		if !errors.Is(err, ErrAuthenticationRejected) {
			t.Fatalf("RunOnce error = %v, want ErrAuthenticationRejected", err)
		}
		var handshakeErr *HandshakeError
		if !errors.As(err, &handshakeErr) || handshakeErr.Retryable() {
			t.Fatalf("authentication rejection should be a permanent HandshakeError: %v", err)
		}

		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		if err != nil {
			t.Fatalf("authentication rejection reserved port %d: %v", port, err)
		}
		_ = listener.Close()
	})

	t.Run("occupied port listener remains intact", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("occupy port: %v", err)
		}
		defer occupied.Close()
		port := uint16(occupied.Addr().(*net.TCPAddr).Port)

		server, err := NewServer(ServerConfig{
			Verify:         func(context.Context, VerifyRequest) error { return nil },
			PublicBindHost: "127.0.0.1",
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		defer server.Close(context.Background())
		tunnelHTTP := httptest.NewServer(server.TunnelHandler())
		defer tunnelHTTP.Close()

		client, err := NewClient(ClientConfig{
			ServerURL:    tunnelHTTP.URL,
			Token:        "secret",
			PublicPort:   port,
			LocalAddress: "127.0.0.1:1",
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		err = client.RunOnce(context.Background())
		if !errors.Is(err, ErrPublicPortInUse) {
			t.Fatalf("RunOnce error = %v, want ErrPublicPortInUse", err)
		}
		var handshakeErr *HandshakeError
		if !errors.As(err, &handshakeErr) || handshakeErr.Retryable() {
			t.Fatalf("port rejection should be a permanent HandshakeError: %v", err)
		}

		connection, err := net.DialTimeout("tcp", occupied.Addr().String(), time.Second)
		if err != nil {
			t.Fatalf("occupied listener was disrupted: %v", err)
		}
		_ = connection.Close()
	})
}

func TestClientReconnectsAfterTemporaryHandshakeFailure(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "online")
	}))
	defer local.Close()

	server, err := NewServer(ServerConfig{
		Verify:         func(context.Context, VerifyRequest) error { return nil },
		PublicBindHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close(context.Background())

	var attempts atomic.Int32
	tunnelHandler := server.TunnelHandler()
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(writer, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		tunnelHandler.ServeHTTP(writer, request)
	}))
	defer gateway.Close()

	statuses := make(chan Status, 16)
	port := freePort(t)
	client, err := NewClient(ClientConfig{
		ServerURL:         gateway.URL,
		Token:             "secret",
		PublicPort:        port,
		LocalAddress:      strings.TrimPrefix(local.URL, "http://"),
		ReconnectMinDelay: 10 * time.Millisecond,
		ReconnectMaxDelay: 20 * time.Millisecond,
		MaxAttempts:       3,
		OnStatus: func(status Status) {
			statuses <- status
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- client.Run(ctx)
	}()
	waitForStatus(t, statuses, StatusOnline)
	if attempts.Load() < 2 {
		t.Fatalf("websocket attempts = %d, want at least 2", attempts.Load())
	}

	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(int(port)))
	if err != nil {
		t.Fatalf("request after reconnect: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "online" {
		t.Fatalf("unexpected response after reconnect: %q", body)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run after cancellation = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestBuildTunnelURL(t *testing.T) {
	got, err := buildTunnelURL("https://example.com/base?keep=yes", "/_reverse/tunnel", "dev.example.com", 3210)
	if err != nil {
		t.Fatalf("buildTunnelURL: %v", err)
	}
	want := "wss://example.com/_reverse/tunnel?domain=dev.example.com&keep=yes&port=3210"
	if got != want {
		t.Fatalf("buildTunnelURL = %q, want %q", got, want)
	}
}

func TestEncodedTokenPreservesWhitespaceAndUnicode(t *testing.T) {
	const token = "  päss word  "
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	request.Header.Set(TokenHeader, "v1."+base64.RawURLEncoding.EncodeToString([]byte(token)))
	if got := requestToken(request); got != token {
		t.Fatalf("requestToken = %q, want %q", got, token)
	}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("release allocated port: %v", err)
	}
	return port
}

func waitForStatus(t *testing.T, statuses <-chan Status, want StatusState) Status {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case status := <-statuses:
			if status.State == want {
				return status
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for status %q", want)
		}
	}
}

func waitForEvent(t *testing.T, events <-chan Event, eventType EventType, path string) Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == eventType && event.Path == path {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for event %q at %q", eventType, path)
		}
	}
}
