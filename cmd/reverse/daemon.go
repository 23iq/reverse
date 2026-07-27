package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/23iq/reverse/internal/cli"
	"github.com/23iq/reverse/internal/config"
	"github.com/23iq/reverse/internal/daemon"
	"github.com/23iq/reverse/internal/tunnel"
)

const (
	daemonStartTimeout = 5 * time.Second
	daemonStopTimeout  = 8 * time.Second
)

type tunnelRuntime struct {
	clientConfig config.Client
	localAddress string
	publicURL    string
	domain       string
}

type tunnelCallbacks struct {
	onEvent   tunnel.EventCallback
	onStatus  tunnel.StatusCallback
	onTraffic tunnel.TrafficCallback
}

func resolveTunnelRuntime(ctx context.Context, options cli.Options) (tunnelRuntime, error) {
	clientConfig, err := config.LoadClient("")
	if err != nil {
		return tunnelRuntime{}, err
	}
	localAddress, err := resolveLocalAddress(ctx, options, clientConfig)
	if err != nil {
		return tunnelRuntime{}, err
	}
	parsedServerURL, err := url.Parse(clientConfig.ServerURL)
	if err != nil {
		return tunnelRuntime{}, fmt.Errorf("parse configured server URL: %w", err)
	}
	return tunnelRuntime{
		clientConfig: clientConfig,
		localAddress: localAddress,
		publicURL:    strings.TrimRight(clientConfig.ServerURL, "/"),
		domain:       parsedServerURL.Hostname(),
	}, nil
}

// resolveLocalAddress is shared by foreground and background tunnels. An
// explicit --host is exact; otherwise legacy 127.0.0.1 configurations migrate
// to localhost so Go's resolver can try both IPv6 and IPv4 loopback addresses.
func resolveLocalAddress(ctx context.Context, options cli.Options, clientConfig config.Client) (string, error) {
	localHost := clientConfig.LocalHost
	if options.Host != "" {
		localHost = options.Host
	} else if localHost == "" || localHost == "127.0.0.1" {
		localHost = config.DefaultLocalHost
	}
	localAddress := net.JoinHostPort(localHost, strconv.Itoa(options.Port))
	if err := checkLocalService(ctx, localAddress); err != nil {
		return "", err
	}
	return localAddress, nil
}

func newTunnelClient(options cli.Options, runtime tunnelRuntime, callbacks tunnelCallbacks) (*tunnel.Client, error) {
	return tunnel.NewClient(tunnel.ClientConfig{
		ServerURL:    runtime.clientConfig.ServerURL,
		Token:        runtime.clientConfig.Password,
		Domain:       runtime.domain,
		PublicPort:   uint16(options.Port),
		LocalAddress: runtime.localAddress,
		OnEvent:      callbacks.onEvent,
		OnStatus:     callbacks.onStatus,
		OnTraffic:    callbacks.onTraffic,
	})
}

func runBackgroundStart(ctx context.Context, options cli.Options, output io.Writer) error {
	if !daemon.Supported() {
		return daemon.ErrUnsupported
	}
	// Fail in the invoking terminal for config and local-target mistakes. The
	// worker independently reloads config; no password is placed in argv/state.
	if _, err := resolveTunnelRuntime(ctx, options); err != nil {
		return err
	}
	paths, err := daemon.RuntimePaths()
	if err != nil {
		return err
	}
	if state, queryErr := daemon.Query(paths); queryErr == nil {
		return fmt.Errorf("%w (PID %d, %s)", daemon.ErrAlreadyRunning, state.PID, state.LocalTarget)
	}

	probe, err := daemon.AcquireLock(paths)
	if err != nil {
		return err
	}
	if err := daemon.CleanupStale(paths); err != nil {
		_ = probe.Close()
		return err
	}
	if err := probe.Close(); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate reverse executable: %w", err)
	}
	arguments := []string{"--daemon-worker", "--port", strconv.Itoa(options.Port)}
	if options.Host != "" {
		arguments = append(arguments, "--host", options.Host)
	}
	command := exec.Command(executable, arguments...)
	command.Dir = paths.Dir
	workingDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("find current directory for background config: %w", err)
	}
	command.Env, err = daemonEnvironment(os.Environ(), workingDir)
	if err != nil {
		return err
	}
	if err := configureDetachedProcess(command); err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start background reverse tunnel: %w", err)
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("detach background reverse tunnel: %w", err)
	}

	deadline := time.Now().Add(daemonStartTimeout)
	for time.Now().Before(deadline) {
		state, queryErr := daemon.Query(paths)
		if queryErr == nil {
			if state.PID != pid {
				return fmt.Errorf("%w (PID %d)", daemon.ErrAlreadyRunning, state.PID)
			}
			_, _ = fmt.Fprintln(output, "Background tunnel started.")
			renderDaemonState(output, state, true)
			_, _ = fmt.Fprintln(output, "Use reverse --status to inspect it and reverse --stop to stop it.")
			return nil
		}
		if stored, readErr := daemon.ReadState(paths); readErr == nil && stored.PID == pid && stored.Status == string(tunnel.StatusStopped) {
			if stored.LastError != "" {
				return errors.New(stored.LastError)
			}
			return errors.New("background reverse tunnel stopped during startup")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("background reverse tunnel did not become ready; run reverse --status for details")
}

func runDaemonWorker(ctx context.Context, options cli.Options) error {
	if !daemon.Supported() {
		return daemon.ErrUnsupported
	}
	paths, err := daemon.RuntimePaths()
	if err != nil {
		return err
	}
	lock, err := daemon.AcquireLock(paths)
	if err != nil {
		return err
	}
	defer lock.Close()

	instanceID, err := daemon.NewInstanceID()
	if err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	runtime, err := resolveTunnelRuntime(ctx, options)
	if err != nil {
		localHost := options.Host
		if localHost == "" {
			localHost = config.DefaultLocalHost
		}
		_ = daemon.WriteState(paths, daemon.State{
			InstanceID:  instanceID,
			PID:         os.Getpid(),
			StartedAt:   startedAt,
			UpdatedAt:   time.Now().UTC(),
			LocalTarget: net.JoinHostPort(localHost, strconv.Itoa(options.Port)),
			Status:      string(tunnel.StatusStopped),
			LastError:   err.Error(),
		})
		return err
	}
	store, err := daemon.NewStore(paths, daemon.State{
		InstanceID:  instanceID,
		PID:         os.Getpid(),
		StartedAt:   startedAt,
		UpdatedAt:   startedAt,
		PublicURL:   runtime.publicURL,
		LocalTarget: runtime.localAddress,
		Status:      "starting",
	})
	if err != nil {
		return err
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	control, err := daemon.ListenControl(paths, instanceID, store.Snapshot, cancel)
	if err != nil {
		store.Update(func(state *daemon.State) {
			state.Status = string(tunnel.StatusStopped)
			state.LastError = err.Error()
		})
		_ = store.Close()
		return err
	}

	client, err := newTunnelClient(options, runtime, tunnelCallbacks{
		onEvent: func(event tunnel.Event) {
			store.Update(func(state *daemon.State) {
				recordDaemonEventTime(state, event.Time)
				switch event.Type {
				case tunnel.EventRequestFinished:
					state.Requests++
				case tunnel.EventRequestError, tunnel.EventTunnelError:
					state.LastError = event.Error
				}
			})
		},
		onStatus: func(status tunnel.Status) {
			store.Update(func(state *daemon.State) {
				state.Status = string(status.State)
				state.Attempt = status.Attempt
				recordDaemonEventTime(state, status.At)
				if status.Error != "" {
					state.LastError = status.Error
				} else if status.State == tunnel.StatusOnline {
					state.LastError = ""
				}
			})
		},
		onTraffic: func(traffic tunnel.Traffic) {
			store.Update(func(state *daemon.State) {
				updateDaemonTrafficState(state, traffic)
			})
		},
	})
	if err == nil {
		err = client.Run(workerCtx)
	}
	if err != nil {
		err = friendlyTunnelError(err, options.Port)
	}

	store.Update(func(state *daemon.State) {
		state.Status = string(tunnel.StatusStopped)
		state.LastEventAt = time.Now().UTC()
		if err != nil {
			state.LastError = err.Error()
		}
	})
	flushErr := store.Flush()
	closeControlErr := control.Close()
	closeStoreErr := store.Close()
	if err == nil {
		// A requested/signal-driven stop has no stale diagnostic to preserve.
		if removeErr := daemon.RemoveState(paths); removeErr != nil {
			return removeErr
		}
	}
	if err != nil {
		return err
	}
	if flushErr != nil {
		return flushErr
	}
	if closeControlErr != nil && !errors.Is(closeControlErr, net.ErrClosed) {
		return closeControlErr
	}
	return closeStoreErr
}

func runDaemonStatus(output io.Writer) error {
	if !daemon.Supported() {
		return daemon.ErrUnsupported
	}
	paths, err := daemon.RuntimePaths()
	if err != nil {
		return err
	}
	state, queryErr := daemon.Query(paths)
	if queryErr == nil {
		renderDaemonState(output, state, true)
		return nil
	}

	stored, readErr := daemon.ReadState(paths)
	probe, lockErr := daemon.AcquireLock(paths)
	if errors.Is(lockErr, daemon.ErrAlreadyRunning) {
		if readErr == nil {
			renderDaemonState(output, stored, true)
		} else {
			_, _ = fmt.Fprintln(output, "Status: running")
			_, _ = fmt.Fprintln(output, "Tunnel status: starting or unavailable")
		}
		_, err = fmt.Fprintf(output, "Control: unavailable (%v)\n", queryErr)
		return err
	}
	if lockErr != nil {
		return lockErr
	}
	defer probe.Close()

	if readErr == nil {
		renderDaemonState(output, stored, false)
		return nil
	}
	if errors.Is(readErr, daemon.ErrNotRunning) {
		_, err = fmt.Fprintln(output, "Status: stopped")
		return err
	}
	_, err = fmt.Fprintf(output, "Status: stopped\nLast error: daemon state is unavailable: %v\n", readErr)
	return err
}

func runDaemonStop(ctx context.Context, output io.Writer) error {
	if !daemon.Supported() {
		return daemon.ErrUnsupported
	}
	paths, err := daemon.RuntimePaths()
	if err != nil {
		return err
	}
	state, stopErr := daemon.Stop(paths)
	if stopErr != nil {
		lock, lockErr := daemon.AcquireLock(paths)
		if lockErr == nil {
			defer lock.Close()
			if err := daemon.CleanupStale(paths); err != nil {
				return err
			}
			_, err := fmt.Fprintln(output, "Background tunnel is already stopped.")
			return err
		}
		if errors.Is(lockErr, daemon.ErrAlreadyRunning) {
			return fmt.Errorf("background worker is active but its identity could not be verified; refusing to signal a PID: %w", stopErr)
		}
		return lockErr
	}

	deadline := time.Now().Add(daemonStopTimeout)
	for time.Now().Before(deadline) {
		lock, lockErr := daemon.AcquireLock(paths)
		if lockErr == nil {
			if cleanupErr := daemon.CleanupStale(paths); cleanupErr != nil {
				_ = lock.Close()
				return cleanupErr
			}
			if closeErr := lock.Close(); closeErr != nil {
				return closeErr
			}
			_, err := fmt.Fprintf(output, "Background tunnel stopped (PID %d).\n", state.PID)
			return err
		}
		if !errors.Is(lockErr, daemon.ErrAlreadyRunning) {
			return lockErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("background reverse tunnel did not stop cleanly")
}

func renderDaemonState(output io.Writer, state daemon.State, running bool) {
	lifecycleLabel := "Last tunnel status"
	pidLabel := "Last PID"
	if running {
		lifecycleLabel = "Tunnel status"
		pidLabel = "PID"
	}
	if running {
		_, _ = fmt.Fprintln(output, "Status: running")
	} else {
		_, _ = fmt.Fprintln(output, "Status: stopped")
	}
	_, _ = fmt.Fprintf(output, "%s: %s\n", lifecycleLabel, state.Status)
	_, _ = fmt.Fprintf(output, "%s: %d\n", pidLabel, state.PID)
	_, _ = fmt.Fprintf(output, "Public URL: %s\n", state.PublicURL)
	_, _ = fmt.Fprintf(output, "Local target: %s\n", state.LocalTarget)
	end := time.Now().UTC()
	if !running && !state.UpdatedAt.IsZero() {
		end = state.UpdatedAt
	}
	if !state.StartedAt.IsZero() {
		_, _ = fmt.Fprintf(output, "Uptime: %s\n", formatDaemonDuration(end.Sub(state.StartedAt)))
	}
	_, _ = fmt.Fprintf(output, "Requests: %d\n", state.Requests)
	_, _ = fmt.Fprintf(output, "Traffic: %s in / %s out\n", formatDaemonBytes(state.BytesIn), formatDaemonBytes(state.BytesOut))
	if state.Attempt > 0 {
		_, _ = fmt.Fprintf(output, "Connection attempt: %d\n", state.Attempt)
	}
	if !state.LastEventAt.IsZero() {
		_, _ = fmt.Fprintf(output, "Last update: %s\n", state.LastEventAt.Local().Format(time.RFC3339))
	}
	if state.LastError != "" {
		_, _ = fmt.Fprintf(output, "Last error: %s\n", state.LastError)
	}
}

func formatDaemonDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Truncate(time.Second).String()
}

func formatDaemonBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor := float64(unit)
	suffix := "KiB"
	if value >= unit*unit {
		divisor = float64(unit * unit)
		suffix = "MiB"
	}
	if value >= unit*unit*unit {
		divisor = float64(unit * unit * unit)
		suffix = "GiB"
	}
	return fmt.Sprintf("%.1f %s", float64(value)/divisor, suffix)
}

func recordDaemonEventTime(state *daemon.State, eventTime time.Time) {
	if eventTime.After(state.LastEventAt) {
		state.LastEventAt = eventTime
	}
}

func updateDaemonTrafficState(state *daemon.State, traffic tunnel.Traffic) {
	if traffic.TotalToLocal > state.BytesIn {
		state.BytesIn = traffic.TotalToLocal
	}
	if traffic.TotalFromLocal > state.BytesOut {
		state.BytesOut = traffic.TotalFromLocal
	}
	recordDaemonEventTime(state, traffic.At)
}

func daemonEnvironment(environment []string, workingDir string) ([]string, error) {
	blocked := map[string]struct{}{
		"REVERSE_DOMAIN":   {},
		"REVERSE_EMAIL":    {},
		"REVERSE_PASSWORD": {},
	}
	sanitized := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			if _, sensitive := blocked[name]; sensitive {
				continue
			}
			if name == "REVERSE_CONFIG" && value != "" && !filepath.IsAbs(value) {
				if !filepath.IsAbs(workingDir) {
					return nil, errors.New("cannot resolve relative REVERSE_CONFIG without an absolute working directory")
				}
				entry = name + "=" + filepath.Clean(filepath.Join(workingDir, value))
			}
		}
		sanitized = append(sanitized, entry)
	}
	return sanitized, nil
}
