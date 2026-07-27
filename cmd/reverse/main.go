package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/23iq/reverse/internal/auth"
	"github.com/23iq/reverse/internal/buildinfo"
	"github.com/23iq/reverse/internal/cli"
	"github.com/23iq/reverse/internal/config"
	"github.com/23iq/reverse/internal/setup"
	"github.com/23iq/reverse/internal/tunnel"
	"github.com/23iq/reverse/internal/ui"
)

const healthPath = "/_reverse/health"

func main() {
	options, err := cli.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "reverse:", err)
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, cli.Help)
		os.Exit(2)
	}
	if err := run(context.Background(), options, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, ui.RenderError(err))
		os.Exit(1)
	}
}

func run(parent context.Context, options cli.Options, output io.Writer) error {
	switch options.Action {
	case cli.ActionHelp:
		_, err := fmt.Fprint(output, cli.Help)
		return err
	case cli.ActionVersion:
		_, err := fmt.Fprintf(output, "reverse %s\n", buildinfo.Version)
		return err
	case cli.ActionConfigure:
		return runConfigure(parent, output)
	case cli.ActionSetup:
		ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runSetup(ctx, options, output)
	case cli.ActionTunnel:
		ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
		defer stop()
		if options.Background {
			return runBackgroundStart(ctx, options, output)
		}
		return runTunnel(ctx, options)
	case cli.ActionStatus:
		return runDaemonStatus(output)
	case cli.ActionStop:
		return runDaemonStop(parent, output)
	case cli.ActionServer:
		ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runServer(ctx, options.ServerConfig, output)
	case cli.ActionDaemonWorker:
		ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemonWorker(ctx, options)
	default:
		return errors.New("unknown command")
	}
}

func runConfigure(ctx context.Context, output io.Writer) error {
	domain, password, err := credentials(ui.ClientConfigForm)
	if errors.Is(err, ui.ErrCancelled) {
		_, _ = fmt.Fprintln(output, "Configuration cancelled.")
		return nil
	}
	if err != nil {
		return err
	}
	serverURL, err := config.NormalizeServerURL(domain)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}

	path, err := config.DefaultClientPath()
	if err != nil {
		return err
	}
	clientConfig := config.Client{
		ServerURL: serverURL,
		Password:  password,
		LocalHost: config.DefaultLocalHost,
	}
	if err := config.SaveClient(path, clientConfig); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(output, "Configuration saved to %s\n", path)
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := probeServer(probeCtx, serverURL); err != nil {
		_, _ = fmt.Fprintf(output, "Warning: configuration was saved, but the server check failed: %v\n", err)
	} else {
		_, _ = fmt.Fprintf(output, "Server is reachable at %s\n", serverURL)
	}
	return nil
}

type credentialForm func() (string, string, error)

func credentials(form credentialForm) (string, string, error) {
	domain, domainSet := os.LookupEnv("REVERSE_DOMAIN")
	password, passwordSet := os.LookupEnv("REVERSE_PASSWORD")
	if domainSet || passwordSet {
		if !domainSet || !passwordSet {
			return "", "", errors.New("REVERSE_DOMAIN and REVERSE_PASSWORD must be set together")
		}
		return domain, password, nil
	}

	info, err := os.Stdin.Stat()
	if err != nil {
		return "", "", fmt.Errorf("inspect terminal: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return "", "", errors.New("an interactive terminal is required; set REVERSE_DOMAIN and REVERSE_PASSWORD for non-interactive use")
	}
	return form()
}

func probeServer(ctx context.Context, serverURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+healthPath, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

func runTunnel(ctx context.Context, options cli.Options) error {
	runtime, err := resolveTunnelRuntime(ctx, options)
	if err != nil {
		return err
	}
	bridge := newDashboardBridge(ctx, runtime.publicURL, runtime.localAddress)

	client, err := newTunnelClient(options, runtime, tunnelCallbacks{
		onEvent:   bridge.onTunnelEvent,
		onStatus:  bridge.onTunnelStatus,
		onTraffic: bridge.onTunnelTraffic,
	})
	if err != nil {
		bridge.close()
		return err
	}

	tunnelResult := make(chan error, 1)
	go func() {
		runErr := client.Run(bridge.ctx)
		if runErr != nil {
			bridge.send(ui.Event{
				Kind:    ui.EventError,
				Time:    time.Now(),
				Message: friendlyTunnelError(runErr, options.Port).Error(),
			})
		}
		tunnelResult <- runErr
		bridge.close()
	}()

	dashboardErr := ui.RunDashboard(bridge.events, ui.DashboardOptions{
		PublicURL:   runtime.publicURL,
		LocalTarget: runtime.localAddress,
		StartTime:   time.Now(),
		MaxLogLines: 2000,
	})
	bridge.cancel()
	if dashboardErr != nil {
		return dashboardErr
	}

	select {
	case runErr := <-tunnelResult:
		if runErr != nil {
			return friendlyTunnelError(runErr, options.Port)
		}
	case <-time.After(5 * time.Second):
		return errors.New("tunnel did not stop cleanly")
	}
	return nil
}

func checkLocalService(ctx context.Context, address string) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("nothing is listening at %s: %w", address, err)
	}
	return connection.Close()
}

func friendlyTunnelError(err error, port int) error {
	switch {
	case errors.Is(err, tunnel.ErrAuthenticationRejected):
		return errors.New("authentication failed; run reverse --config and check the domain and password")
	case errors.Is(err, tunnel.ErrPublicPortInUse):
		return fmt.Errorf("port %d is already occupied on the VPS; reverse did not change the existing listener", port)
	case errors.Is(err, tunnel.ErrTunnelBusy):
		return errors.New("another reverse client is already connected to this server")
	default:
		return err
	}
}

type dashboardBridge struct {
	ctx        context.Context
	cancelFunc context.CancelFunc
	events     chan ui.Event
	stop       chan struct{}
	stopped    chan struct{}
	closeOnce  sync.Once
	incoming   atomic.Int64
	outgoing   atomic.Int64
	publicURL  string
	target     string
}

func newDashboardBridge(parent context.Context, publicURL, target string) *dashboardBridge {
	ctx, cancel := context.WithCancel(parent)
	bridge := &dashboardBridge{
		ctx:        ctx,
		cancelFunc: cancel,
		events:     make(chan ui.Event, 512),
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
		publicURL:  publicURL,
		target:     target,
	}
	go bridge.flushTraffic()
	return bridge
}

func (b *dashboardBridge) cancel() {
	b.cancelFunc()
}

func (b *dashboardBridge) send(event ui.Event) {
	select {
	case b.events <- event:
	case <-b.ctx.Done():
	}
}

func (b *dashboardBridge) onTunnelTraffic(traffic tunnel.Traffic) {
	switch traffic.Direction {
	case tunnel.TrafficToLocal:
		b.incoming.Add(traffic.Bytes)
	case tunnel.TrafficFromLocal:
		b.outgoing.Add(traffic.Bytes)
	}
}

func (b *dashboardBridge) flushTraffic() {
	defer close(b.stopped)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.flushTrafficOnce()
		case <-b.stop:
			b.flushTrafficOnce()
			return
		case <-b.ctx.Done():
			return
		}
	}
}

func (b *dashboardBridge) flushTrafficOnce() {
	incoming := b.incoming.Swap(0)
	outgoing := b.outgoing.Swap(0)
	if incoming == 0 && outgoing == 0 {
		return
	}
	b.send(ui.Event{
		Kind:     ui.EventTraffic,
		Time:     time.Now(),
		BytesIn:  incoming,
		BytesOut: outgoing,
	})
}

func (b *dashboardBridge) onTunnelStatus(status tunnel.Status) {
	message := string(status.State)
	if status.Attempt > 1 {
		message += fmt.Sprintf(" (attempt %d)", status.Attempt)
	}
	if status.Error != "" {
		message += ": " + status.Error
	}
	b.send(ui.Event{
		Kind:        ui.EventStatus,
		Time:        status.At,
		Online:      status.State == tunnel.StatusOnline,
		URL:         b.publicURL,
		LocalTarget: b.target,
		Message:     message,
	})
}

func (b *dashboardBridge) onTunnelEvent(event tunnel.Event) {
	switch event.Type {
	case tunnel.EventRequestFinished:
		b.send(ui.Event{
			Kind:           ui.EventRequest,
			Time:           event.Time,
			RemoteAddr:     event.RemoteAddr,
			Method:         event.Method,
			Path:           event.Path,
			StatusCode:     event.Status,
			Duration:       time.Duration(event.Duration) * time.Millisecond,
			BytesIn:        event.BytesIn,
			BytesOut:       event.BytesOut,
			TrafficCounted: true,
		})
	case tunnel.EventRequestError, tunnel.EventTunnelError:
		b.send(ui.Event{
			Kind:    ui.EventError,
			Time:    event.Time,
			Message: event.Error,
		})
	}
}

func (b *dashboardBridge) close() {
	b.closeOnce.Do(func() {
		close(b.stop)
		<-b.stopped
		close(b.events)
	})
}

func runServer(ctx context.Context, path string, output io.Writer) error {
	serverConfig, err := config.LoadServer(path)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tunnelServer, err := tunnel.NewServer(tunnel.ServerConfig{
		PublicBindHost: serverConfig.DirectBind,
		OriginPatterns: []string{serverConfig.Domain},
		Verify: func(_ context.Context, request tunnel.VerifyRequest) error {
			if canonicalDomain(request.Domain) != canonicalDomain(serverConfig.Domain) {
				return tunnel.ErrAuthenticationRejected
			}
			if err := auth.ComparePassword(serverConfig.PasswordHash, request.Token); err != nil {
				return tunnel.ErrAuthenticationRejected
			}
			return nil
		},
		OnEvent: func(event tunnel.Event) {
			logServerEvent(logger, event)
		},
		OnStatus: func(status tunnel.Status) {
			logger.Info("tunnel status",
				"state", status.State,
				"remote_port", status.PublicPort,
				"error", status.Error,
			)
		},
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(healthPath, tunnelServer.HealthHandler())
	mux.Handle("/", tunnelServer.Handler())
	httpServer := &http.Server{
		Addr:              serverConfig.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serverCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-serverCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Drain requests that arrived through Caddy before closing the yamux
		// session they are using. WebSocket connections are hijacked, so
		// Shutdown does not wait for the tunnel itself.
		_ = httpServer.Shutdown(shutdownCtx)
		_ = tunnelServer.Close(shutdownCtx)
	}()

	logger.Info("reverse server started",
		"domain", serverConfig.Domain,
		"listen", serverConfig.Listen,
		"direct_bind", serverConfig.DirectBind,
		"version", buildinfo.Version,
	)
	err = httpServer.ListenAndServe()
	stopServer()
	<-shutdownDone
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve gateway: %w", err)
	}
	return nil
}

func canonicalDomain(domain string) string {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if host, _, err := net.SplitHostPort(domain); err == nil {
		domain = host
	}
	return strings.TrimSuffix(domain, ".")
}

func logServerEvent(logger *slog.Logger, event tunnel.Event) {
	switch event.Type {
	case tunnel.EventRequestFinished:
		logger.Info("request",
			"id", event.ID,
			"remote", event.RemoteAddr,
			"method", event.Method,
			"host", event.Host,
			"path", event.Path,
			"status", event.Status,
			"duration_ms", event.Duration,
			"bytes_in", event.BytesIn,
			"bytes_out", event.BytesOut,
		)
	case tunnel.EventRequestError, tunnel.EventTunnelError:
		logger.Error("tunnel event",
			"type", event.Type,
			"id", event.ID,
			"error", event.Error,
		)
	}
}

func runSetup(ctx context.Context, options cli.Options, output io.Writer) error {
	domain, password, err := credentials(ui.SetupForm)
	if errors.Is(err, ui.ErrCancelled) {
		_, _ = fmt.Fprintln(output, "Setup cancelled.")
		return nil
	}
	if err != nil {
		return err
	}
	serverURL, err := config.NormalizeServerURL(domain)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return err
	}
	if parsed.Port() != "" {
		return errors.New("setup domain cannot include a port")
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}

	setupOptions := setup.Options{
		Domain:   parsed.Hostname(),
		Password: password,
		Email:    strings.TrimSpace(os.Getenv("REVERSE_EMAIL")),
		DryRun:   options.DryRun,
		RootDir:  options.SetupRoot,
	}
	if terminalAttached(os.Stdin) && terminalAttached(os.Stdout) {
		err = ui.RunSetupProgress(ctx, func(runCtx context.Context, events chan<- ui.ProgressEvent) error {
			return setup.Run(runCtx, setupOptions, func(progress setup.Progress) {
				event := ui.ProgressEvent{
					Stage:   string(progress.Stage),
					Status:  progressStatus(progress.Status),
					Message: progress.Message,
					Command: progress.Command,
				}
				select {
				case events <- event:
				case <-runCtx.Done():
				}
			})
		})
	} else {
		err = setup.Run(ctx, setupOptions, func(progress setup.Progress) {
			if progress.Status == setup.StatusSuccess && progress.Stage != setup.StageComplete {
				return
			}
			_, _ = fmt.Fprintf(output, "[%s] %s\n", progress.Stage, progress.Message)
			if progress.Command != "" && options.DryRun {
				_, _ = fmt.Fprintf(output, "  %s\n", progress.Command)
			}
		})
	}
	if err != nil {
		return err
	}
	if options.DryRun {
		_, _ = fmt.Fprintln(output, "Dry run complete. No changes were made.")
		return nil
	}
	_, _ = fmt.Fprintf(output, "REVERSE is ready at %s\n", serverURL)
	return nil
}

func terminalAttached(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func progressStatus(status setup.Status) ui.ProgressStatus {
	switch status {
	case setup.StatusSuccess:
		return ui.ProgressDone
	case setup.StatusWarning:
		return ui.ProgressWarning
	default:
		return ui.ProgressRunning
	}
}
