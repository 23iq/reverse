package setup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/23iq/reverse/internal/auth"
)

type rollbackAction struct {
	message string
	run     func(context.Context) error
}

type rollbackStack struct {
	actions []rollbackAction
}

func (stack *rollbackStack) add(message string, run func(context.Context) error) {
	stack.actions = append(stack.actions, rollbackAction{message: message, run: run})
}

func (stack *rollbackStack) run(ctx context.Context, progress ProgressFunc) error {
	var result error
	for index := len(stack.actions) - 1; index >= 0; index-- {
		action := stack.actions[index]
		emit(progress, StageRollback, StatusRunning, action.message, "")
		if err := action.run(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("%s: %w", action.message, err))
			emit(progress, StageRollback, StatusWarning, action.message+": "+err.Error(), "")
		}
	}
	return result
}

func Run(ctx context.Context, options Options, progress ProgressFunc) (runErr error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return err
	}
	emit(progress, StageValidate, StatusSuccess, "Setup values are valid", "")

	if opts.Runner == nil {
		opts.Runner = execRunner{}
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.PublicIPSource == nil {
		opts.PublicIPSource = publicIPDetector{}
	}
	if opts.PortChecker == nil {
		opts.PortChecker = bindPortChecker{}
	}
	if opts.ReadinessChecker == nil {
		opts.ReadinessChecker = httpReadinessChecker{}
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.EffectiveUID == nil {
		opts.EffectiveUID = os.Geteuid
	}
	if !opts.DryRun && opts.EffectiveUID() != 0 {
		return errors.New("reverse --setup must run as root")
	}

	manager, err := detectPackageManager(opts)
	if err != nil {
		return err
	}
	if opts.DryRun {
		return runDryPlan(opts, manager, progress)
	}
	if info, statErr := os.Stat(filepath.Join(opts.SourceDir, "Dockerfile")); statErr != nil || info.IsDir() {
		if statErr == nil {
			statErr = errors.New("path is a directory")
		}
		return fmt.Errorf("source directory must contain Dockerfile: %w", statErr)
	}

	emit(progress, StageDNS, StatusRunning, "Checking DNS records", "")
	if err := verifyDNS(ctx, opts, progress); err != nil {
		return err
	}
	emit(progress, StageDNS, StatusSuccess, "DNS points to this VPS", "")

	caddyPath := rootPath(opts.RootDir, "/etc/caddy/Caddyfile")
	managedCaddy := hasManagedMarker(caddyPath)
	if caddyInfo, caddyErr := os.Lstat(caddyPath); caddyErr == nil {
		if !caddyInfo.Mode().IsRegular() {
			return fmt.Errorf("existing Caddyfile %s is not a regular file; refusing to replace it", caddyPath)
		}
		if !managedCaddy {
			return fmt.Errorf("existing Caddyfile %s is not managed by reverse; refusing to replace it", caddyPath)
		}
	} else if !os.IsNotExist(caddyErr) {
		return fmt.Errorf("inspect existing Caddyfile %s: %w", caddyPath, caddyErr)
	}
	caddyWasInstalled := pathExists(opts.LookPath, "caddy")
	caddyInitialState, err := inspectServiceState(ctx, opts.Runner, "caddy")
	if err != nil {
		return fmt.Errorf("inspect Caddy service: %w", err)
	}
	if caddyInitialState.active && !managedCaddy {
		return errors.New("an unmanaged Caddy service is active; refusing to stop or replace it")
	}

	dockerInitialState, err := inspectServiceState(ctx, opts.Runner, "docker")
	if err != nil {
		return fmt.Errorf("inspect Docker service: %w", err)
	}
	certbotTimerInitialStates := make(map[string]serviceState, 2)
	for _, unit := range []string{"certbot.timer", "certbot-renew.timer"} {
		state, stateErr := inspectServiceState(ctx, opts.Runner, unit)
		if stateErr != nil {
			return fmt.Errorf("inspect %s: %w", unit, stateErr)
		}
		certbotTimerInitialStates[unit] = state
	}
	container, err := inspectContainer(ctx, opts, dockerInitialState.active)
	if err != nil {
		return err
	}
	if container.exists && !container.managed {
		return fmt.Errorf("container %q already exists and is not managed by reverse", opts.ContainerName)
	}
	allowed := map[string]bool{}
	if managedCaddy && caddyInitialState.active {
		allowed[":80"] = true
		allowed[":443"] = true
	}
	if container.managed && container.running {
		allowed["127.0.0.1:8787"] = true
	}
	if err := checkPorts(ctx, opts, allowed, progress); err != nil {
		return err
	}

	rollback := &rollbackStack{}
	rollback.add("Restore the previous Caddy service state", func(ctx context.Context) error {
		return restoreServiceState(ctx, opts.Runner, "caddy", caddyInitialState)
	})
	rollback.add("Restore the previous Docker service state", func(ctx context.Context) error {
		return restoreServiceState(ctx, opts.Runner, "docker", dockerInitialState)
	})
	defer func() {
		if runErr == nil {
			return
		}
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		if rollbackErr := rollback.run(rollbackContext, progress); rollbackErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("rollback was incomplete: %w", rollbackErr))
		}
	}()

	for _, command := range mustInstallCommands(manager) {
		if err := execute(ctx, opts.Runner, progress, StagePackages, "Installing required packages", command); err != nil {
			return err
		}
	}
	if err := execute(ctx, opts.Runner, progress, StagePackages, "Enabling Docker", Command{
		Name: "systemctl", Args: []string{"enable", "--now", "docker"},
	}); err != nil {
		return err
	}

	container, err = inspectContainer(ctx, opts, true)
	if err != nil {
		return err
	}
	if container.exists && !container.managed {
		return fmt.Errorf("container %q already exists and is not managed by reverse", opts.ContainerName)
	}

	caddyNowActive, err := serviceActive(ctx, opts.Runner, "caddy")
	if err != nil {
		return fmt.Errorf("inspect Caddy service after installation: %w", err)
	}
	if caddyNowActive && caddyWasInstalled && !managedCaddy {
		return errors.New("Caddy became active with an unmanaged configuration; refusing to stop it")
	}

	certificatePath := rootPath(opts.RootDir, filepath.Join("/etc/letsencrypt/live", opts.Domain, "fullchain.pem"))
	privateKeyPath := rootPath(opts.RootDir, filepath.Join("/etc/letsencrypt/live", opts.Domain, "privkey.pem"))
	if certificateUsable(certificatePath, privateKeyPath, opts.Domain, time.Now()) {
		emit(progress, StageCertificate, StatusSuccess, "Reusing the existing TLS certificate", "")
		if caddyInitialState.active && !caddyNowActive {
			if err := execute(ctx, opts.Runner, progress, StageCaddy, "Restarting the existing Caddy service", Command{
				Name: "systemctl", Args: []string{"start", "caddy"},
			}); err != nil {
				return err
			}
			caddyNowActive = true
		}
	} else {
		if caddyNowActive {
			if err := execute(ctx, opts.Runner, progress, StagePorts, "Temporarily stopping Caddy for Certbot", Command{
				Name: "systemctl", Args: []string{"stop", "caddy"},
			}); err != nil {
				return err
			}
			caddyNowActive = false
		}
		certificateAllowed := map[string]bool{}
		if container.managed && container.running {
			certificateAllowed["127.0.0.1:8787"] = true
		}
		if err := checkPorts(ctx, opts, certificateAllowed, progress); err != nil {
			return err
		}
		if err := execute(ctx, opts.Runner, progress, StageCertificate, "Obtaining a Let's Encrypt certificate", certbotCommand(opts)); err != nil {
			return err
		}
		if !certificateUsable(certificatePath, privateKeyPath, opts.Domain, time.Now()) {
			return errors.New("Certbot completed without producing a usable certificate and private key")
		}
		if caddyInitialState.active {
			if err := execute(ctx, opts.Runner, progress, StageCaddy, "Restarting the existing Caddy service", Command{
				Name: "systemctl", Args: []string{"start", "caddy"},
			}); err != nil {
				return err
			}
			caddyNowActive = true
		}
	}
	serverPath := rootPath(opts.RootDir, "/etc/reverse/server.json")
	previousDomain := previousSetupDomain(serverPath, caddyPath)
	if previousDomain != opts.Domain {
		rollback.add("Revoke certificate access for the new domain", func(ctx context.Context) error {
			return revokeCertificateACL(ctx, opts, opts.Domain, previousDomain == "", nil)
		})
	}
	if err := configureCertificateACL(ctx, opts, progress); err != nil {
		return err
	}
	for _, hook := range renderRenewalHooks(opts.Domain, opts.RootDir) {
		restoreHook, hookErr := atomicReplace(hook.path, hook.data, 0o750)
		if hookErr != nil {
			return fmt.Errorf("write certificate renewal hook %s: %w", hook.path, hookErr)
		}
		hookPath := hook.path
		rollback.add("Restore the previous certificate renewal hook "+hookPath, func(context.Context) error {
			return restoreHook()
		})
	}
	emit(progress, StageCertificate, StatusSuccess, "Installed Certbot pre, deploy, and post hooks", "")
	if timerUnit := enableCertbotTimer(ctx, opts, progress); timerUnit != "" {
		timerInitialState := certbotTimerInitialStates[timerUnit]
		rollback.add("Restore the previous "+timerUnit+" state", func(ctx context.Context) error {
			return restoreServiceState(ctx, opts.Runner, timerUnit, timerInitialState)
		})
	}

	passwordHash, err := auth.HashPassword(opts.Password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	serverData, err := renderServerConfig(opts.Domain, passwordHash)
	if err != nil {
		return err
	}
	if err := execute(ctx, opts.Runner, progress, StageContainer, "Building the reverse server image", dockerBuildCommand(opts)); err != nil {
		return err
	}

	backupContainer := ""
	containerRenamed := false
	dockerRunAttempted := false
	rollback.add("Restore the previous reverse container state", func(ctx context.Context) error {
		var rollbackErr error
		if dockerRunAttempted || containerRenamed {
			output, removeErr := opts.Runner.Run(ctx, Command{Name: "docker", Args: []string{"rm", "-f", opts.ContainerName}})
			if removeErr != nil && !isMissingContainerError(output, removeErr) {
				rollbackErr = errors.Join(rollbackErr, removeErr)
			}
		}
		if containerRenamed {
			_, renameErr := opts.Runner.Run(ctx, Command{Name: "docker", Args: []string{"rename", backupContainer, opts.ContainerName}})
			rollbackErr = errors.Join(rollbackErr, renameErr)
			if container.running {
				_, startErr := opts.Runner.Run(ctx, Command{Name: "docker", Args: []string{"start", opts.ContainerName}})
				rollbackErr = errors.Join(rollbackErr, startErr)
			}
		} else if container.running {
			_, startErr := opts.Runner.Run(ctx, Command{Name: "docker", Args: []string{"start", opts.ContainerName}})
			rollbackErr = errors.Join(rollbackErr, startErr)
		}
		return rollbackErr
	})
	if container.managed {
		backupContainer = fmt.Sprintf("%s-backup-%d", opts.ContainerName, time.Now().UnixNano())
		if container.running {
			if err := execute(ctx, opts.Runner, progress, StageContainer, "Stopping the previous reverse server for cutover", Command{
				Name: "docker", Args: []string{"stop", opts.ContainerName},
			}); err != nil {
				return err
			}
		}
	}

	caddyNowActive, err = serviceActive(ctx, opts.Runner, "caddy")
	if err != nil {
		return fmt.Errorf("inspect Caddy service before cutover: %w", err)
	}
	cutoverAllowed := map[string]bool{}
	if caddyNowActive {
		cutoverAllowed[":80"] = true
		cutoverAllowed[":443"] = true
	}
	if err := checkPorts(ctx, opts, cutoverAllowed, progress); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(serverPath), 0o750); err != nil {
		return fmt.Errorf("create reverse config directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(serverPath), 0o750); err != nil {
		return fmt.Errorf("secure reverse config directory: %w", err)
	}
	restoreServer, err := atomicReplace(serverPath, serverData, 0o600)
	if err != nil {
		return fmt.Errorf("write server config: %w", err)
	}
	rollback.add("Restore the previous server config", func(context.Context) error { return restoreServer() })
	emit(progress, StageConfig, StatusSuccess, "Wrote "+serverPath+" with mode 0600", "")

	if container.managed {
		if err := execute(ctx, opts.Runner, progress, StageContainer, "Preserving the previous reverse container", Command{
			Name: "docker", Args: []string{"rename", opts.ContainerName, backupContainer},
		}); err != nil {
			return err
		}
		containerRenamed = true
	}
	dockerRunAttempted = true
	if err := execute(ctx, opts.Runner, progress, StageContainer, "Starting the reverse server", dockerRunCommand(opts, serverPath)); err != nil {
		return err
	}
	emit(progress, StageContainer, StatusRunning, "Waiting for the reverse server health endpoint", "")
	readinessContext, cancelReadiness := context.WithTimeout(ctx, 15*time.Second)
	readinessErr := opts.ReadinessChecker.Wait(readinessContext, "127.0.0.1:8787")
	cancelReadiness()
	if readinessErr != nil {
		return fmt.Errorf("reverse server did not become ready: %w", readinessErr)
	}
	emit(progress, StageContainer, StatusSuccess, "The reverse server is healthy", "")

	restoreCaddy, err := atomicReplace(caddyPath, renderCaddyfile(opts.Domain, opts.RootDir), 0o644)
	if err != nil {
		return fmt.Errorf("write Caddyfile: %w", err)
	}
	rollback.add("Restore the previous Caddyfile", func(ctx context.Context) error {
		restoreErr := restoreCaddy()
		active, inspectErr := serviceActive(ctx, opts.Runner, "caddy")
		if inspectErr != nil || !active {
			return errors.Join(restoreErr, inspectErr)
		}
		_, reloadErr := opts.Runner.Run(ctx, Command{Name: "systemctl", Args: []string{"reload", "caddy"}})
		return errors.Join(restoreErr, reloadErr)
	})
	emit(progress, StageConfig, StatusSuccess, "Wrote "+caddyPath, "")
	if err := execute(ctx, opts.Runner, progress, StageCaddy, "Validating the Caddyfile", Command{
		Name: "caddy", Args: []string{"validate", "--config", caddyPath, "--adapter", "caddyfile"},
	}); err != nil {
		return err
	}
	if err := execute(ctx, opts.Runner, progress, StageCaddy, "Starting Caddy", Command{
		Name: "systemctl", Args: []string{"enable", "--now", "caddy"},
	}); err != nil {
		return err
	}
	if err := execute(ctx, opts.Runner, progress, StageCaddy, "Reloading Caddy", Command{
		Name: "systemctl", Args: []string{"reload", "caddy"},
	}); err != nil {
		return err
	}

	if backupContainer != "" {
		if err := execute(ctx, opts.Runner, progress, StageContainer, "Removing the previous reverse container", Command{
			Name: "docker", Args: []string{"rm", backupContainer},
		}); err != nil {
			emit(progress, StageContainer, StatusWarning, "The old container remains as "+backupContainer, "")
		}
	}
	if previousDomain != "" && previousDomain != opts.Domain {
		if err := revokeCertificateACL(ctx, opts, previousDomain, false, progress); err != nil {
			emit(progress, StageCertificate, StatusWarning, "Could not revoke obsolete certificate access for "+previousDomain+": "+err.Error(), "")
		}
	}
	rollback.actions = nil
	emit(progress, StageComplete, StatusSuccess, "Reverse is ready at https://"+opts.Domain, "")
	return nil
}

type containerState struct {
	exists  bool
	managed bool
	running bool
}

func inspectContainer(ctx context.Context, opts Options, dockerAvailable bool) (containerState, error) {
	if !dockerAvailable {
		return containerState{}, nil
	}
	output, err := opts.Runner.Run(ctx, Command{
		Name: "docker",
		Args: []string{
			"container", "inspect",
			"--format", `{{index .Config.Labels "io.reverse.managed"}}|{{.State.Running}}`,
			opts.ContainerName,
		},
	})
	if err != nil {
		if isMissingContainerError(output, err) {
			return containerState{}, nil
		}
		return containerState{}, fmt.Errorf("inspect existing container: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(output), "|")
	if len(parts) != 2 {
		return containerState{}, fmt.Errorf("unexpected Docker inspect output %q", strings.TrimSpace(output))
	}
	return containerState{
		exists:  true,
		managed: parts[0] == "true",
		running: parts[1] == "true",
	}, nil
}

func isMissingContainerError(output string, err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(output + " " + err.Error())
	return strings.Contains(lower, "no such") || strings.Contains(lower, "not found")
}

func verifyDNS(ctx context.Context, opts Options, progress ProgressFunc) error {
	records, err := opts.Resolver.LookupIPAddr(ctx, opts.Domain)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", opts.Domain, err)
	}
	resolved := make(map[netip.Addr]struct{})
	hasPublicRecord := false
	for _, record := range records {
		if ip, ok := netip.AddrFromSlice(record.IP); ok {
			ip = ip.Unmap()
			resolved[ip] = struct{}{}
			if isPublicIP(ip) {
				hasPublicRecord = true
			}
		}
	}
	if !hasPublicRecord {
		return fmt.Errorf("%s has no public A or AAAA record", opts.Domain)
	}
	public, err := opts.PublicIPSource.PublicIPs(ctx)
	if err != nil || len(public) == 0 {
		emit(progress, StageDNS, StatusWarning, "DNS resolves publicly, but the VPS public IP could not be detected", "")
		return nil
	}
	local := make(map[netip.Addr]struct{}, len(public))
	for _, address := range public {
		local[address.Unmap()] = struct{}{}
	}
	var unmatched []string
	for address := range resolved {
		if _, ok := local[address]; !ok {
			unmatched = append(unmatched, address.String())
		}
	}
	if len(unmatched) == 0 {
		return nil
	}
	sort.Strings(unmatched)
	if opts.AllowDNSMismatch {
		emit(progress, StageDNS, StatusWarning, "Some DNS records do not match this VPS; continuing because mismatch is allowed", "")
		return nil
	}
	return fmt.Errorf("%s has public DNS records that do not belong to this VPS: %s", opts.Domain, strings.Join(unmatched, ", "))
}

func detectPackageManager(opts Options) (PackageManager, error) {
	if opts.PackageManager != "" {
		return opts.PackageManager, nil
	}
	for _, candidate := range []struct {
		binary  string
		manager PackageManager
	}{
		{"apt-get", PackageManagerAPT},
		{"dnf", PackageManagerDNF},
		{"pacman", PackageManagerPacman},
	} {
		if _, err := opts.LookPath(candidate.binary); err == nil {
			return candidate.manager, nil
		}
	}
	return "", errors.New("no supported package manager found; install apt, dnf, or pacman")
}

func mustInstallCommands(manager PackageManager) []Command {
	commands, err := installCommands(manager)
	if err != nil {
		panic(err)
	}
	return commands
}

func serviceActive(ctx context.Context, runner Runner, service string) (bool, error) {
	output, err := runner.Run(ctx, Command{Name: "systemctl", Args: []string{"is-active", service}})
	state := strings.ToLower(strings.TrimSpace(output))
	if state == "active" {
		return true, nil
	}
	combined := state
	if err != nil {
		combined += " " + strings.ToLower(err.Error())
	}
	if state == "inactive" || state == "failed" || state == "unknown" ||
		strings.Contains(combined, "not-found") ||
		strings.Contains(combined, "not found") ||
		strings.Contains(combined, "could not be found") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

type serviceState struct {
	active  bool
	enabled bool
}

func inspectServiceState(ctx context.Context, runner Runner, service string) (serviceState, error) {
	active, err := serviceActive(ctx, runner, service)
	if err != nil {
		return serviceState{}, err
	}
	enabled, err := serviceEnabled(ctx, runner, service)
	if err != nil {
		return serviceState{}, err
	}
	return serviceState{active: active, enabled: enabled}, nil
}

func serviceEnabled(ctx context.Context, runner Runner, service string) (bool, error) {
	output, err := runner.Run(ctx, Command{Name: "systemctl", Args: []string{"is-enabled", service}})
	state := strings.ToLower(strings.TrimSpace(output))
	switch state {
	case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias":
		return true, nil
	case "disabled", "static", "indirect", "generated", "transient",
		"masked", "masked-runtime", "not-found", "unknown":
		return false, nil
	}
	combined := state
	if err != nil {
		combined += " " + strings.ToLower(err.Error())
	}
	if strings.Contains(combined, "not-found") ||
		strings.Contains(combined, "not found") ||
		strings.Contains(combined, "could not be found") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, fmt.Errorf("unexpected systemctl is-enabled state %q for %s", state, service)
}

func restoreServiceState(ctx context.Context, runner Runner, service string, want serviceState) error {
	var result error

	enabled, err := serviceEnabled(ctx, runner, service)
	if err != nil {
		result = errors.Join(result, fmt.Errorf("inspect enablement: %w", err))
	} else if enabled != want.enabled {
		action := "disable"
		if want.enabled {
			action = "enable"
		}
		if _, commandErr := runner.Run(ctx, Command{Name: "systemctl", Args: []string{action, service}}); commandErr != nil {
			result = errors.Join(result, fmt.Errorf("%s service: %w", action, commandErr))
		}
	}

	active, err := serviceActive(ctx, runner, service)
	if err != nil {
		result = errors.Join(result, fmt.Errorf("inspect activity: %w", err))
	} else if active != want.active {
		action := "stop"
		if want.active {
			action = "start"
		}
		if _, commandErr := runner.Run(ctx, Command{Name: "systemctl", Args: []string{action, service}}); commandErr != nil {
			result = errors.Join(result, fmt.Errorf("%s service: %w", action, commandErr))
		}
	}
	return result
}

func pathExists(lookup func(string) (string, error), name string) bool {
	_, err := lookup(name)
	return err == nil
}

func hasManagedMarker(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), caddyManagedMarker)
}

func checkPorts(ctx context.Context, opts Options, allowed map[string]bool, progress ProgressFunc) error {
	for _, address := range []string{":80", ":443", "127.0.0.1:8787"} {
		err := opts.PortChecker.Check(ctx, address)
		if err == nil {
			emit(progress, StagePorts, StatusSuccess, address+" is available", "")
			continue
		}
		if allowed[address] {
			emit(progress, StagePorts, StatusWarning, address+" is held by the existing reverse installation", "")
			continue
		}
		return fmt.Errorf("required address %s is unavailable; reverse will not stop its owner: %w", address, err)
	}
	return nil
}

func certificateUsable(certificatePath, privateKeyPath, domain string, now time.Time) bool {
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil || len(keyData) == 0 {
		return false
	}
	data, err := os.ReadFile(certificatePath)
	if err != nil {
		return false
	}
	if _, err := tls.X509KeyPair(data, keyData); err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || certificate.VerifyHostname(domain) != nil {
		return false
	}
	return certificate.NotBefore.Before(now) && certificate.NotAfter.After(now.Add(7*24*time.Hour))
}

func dockerBuildCommand(opts Options) Command {
	return Command{
		Name: "docker",
		Args: []string{"build", "--pull", "--tag", opts.ServerImage, "."},
		Dir:  opts.SourceDir,
	}
}

func dockerRunCommand(opts Options, serverPath string) Command {
	return Command{
		Name: "docker",
		Args: []string{
			"run", "--detach",
			"--name", opts.ContainerName,
			"--label", "io.reverse.managed=true",
			"--network", "host",
			"--restart", "unless-stopped",
			"--stop-timeout", "15",
			"--pids-limit", "256",
			"--log-driver", "json-file",
			"--log-opt", "max-size=10m",
			"--log-opt", "max-file=3",
			"--read-only",
			"--security-opt", "no-new-privileges:true",
			"--cap-drop", "ALL",
			"--cap-add", "NET_BIND_SERVICE",
			"--cap-add", "SETGID",
			"--cap-add", "SETPCAP",
			"--cap-add", "SETUID",
			"--volume", serverPath + ":/etc/reverse/server.json:ro,Z",
			opts.ServerImage,
		},
	}
}

func certificateACLCommands(opts Options) []Command {
	letsencrypt := rootPath(opts.RootDir, "/etc/letsencrypt")
	live := rootPath(opts.RootDir, "/etc/letsencrypt/live")
	archive := rootPath(opts.RootDir, "/etc/letsencrypt/archive")
	lineage := rootPath(opts.RootDir, filepath.Join("/etc/letsencrypt/live", opts.Domain))
	archiveLineage := rootPath(opts.RootDir, filepath.Join("/etc/letsencrypt/archive", opts.Domain))
	return []Command{
		{Name: "setfacl", Args: []string{"--modify", "u:caddy:--x", letsencrypt}},
		{Name: "setfacl", Args: []string{"--modify", "u:caddy:--x", live, archive}},
		{Name: "setfacl", Args: []string{"--recursive", "--modify", "u:caddy:rX", lineage, archiveLineage}},
		{Name: "setfacl", Args: []string{"--default", "--modify", "u:caddy:rX", archiveLineage}},
	}
}

func configureCertificateACL(ctx context.Context, opts Options, progress ProgressFunc) error {
	for _, command := range certificateACLCommands(opts) {
		if err := execute(ctx, opts.Runner, progress, StageCertificate, "Granting Caddy read-only certificate access", command); err != nil {
			return err
		}
	}
	return nil
}

func enableCertbotTimer(ctx context.Context, opts Options, progress ProgressFunc) string {
	for _, unit := range []string{"certbot.timer", "certbot-renew.timer"} {
		command := Command{Name: "systemctl", Args: []string{"enable", "--now", unit}}
		emit(progress, StageCertificate, StatusRunning, "Enabling automatic certificate renewal", command.String())
		if _, err := opts.Runner.Run(ctx, command); err == nil {
			emit(progress, StageCertificate, StatusSuccess, "Automatic certificate renewal is enabled", command.String())
			return unit
		}
	}
	emit(progress, StageCertificate, StatusWarning, "Certbot has no known systemd timer; configure `certbot renew` in cron", "")
	return ""
}

func execute(ctx context.Context, runner Runner, progress ProgressFunc, stage Stage, message string, command Command) error {
	emit(progress, stage, StatusRunning, message, command.String())
	if _, err := runner.Run(ctx, command); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	emit(progress, stage, StatusSuccess, message, command.String())
	return nil
}

func emit(progress ProgressFunc, stage Stage, status Status, message, command string) {
	if progress != nil {
		progress(Progress{Stage: stage, Status: status, Message: message, Command: command})
	}
}

func runDryPlan(opts Options, manager PackageManager, progress ProgressFunc) error {
	emit(progress, StageValidate, StatusWarning, "Dry run: no DNS lookup, port bind, file write, or command execution will occur", "")
	for _, command := range mustInstallCommands(manager) {
		emit(progress, StagePackages, StatusPending, "Install required packages", command.String())
	}
	emit(progress, StagePackages, StatusPending, "Enable Docker", Command{Name: "systemctl", Args: []string{"enable", "--now", "docker"}}.String())
	emit(progress, StagePorts, StatusPending, "Stop setup-managed Caddy while checking ports", Command{Name: "systemctl", Args: []string{"stop", "caddy"}}.String())
	emit(progress, StageCertificate, StatusPending, "Obtain or reuse a TLS certificate", certbotCommand(opts).String())
	for _, command := range certificateACLCommands(opts) {
		emit(progress, StageCertificate, StatusPending, "Grant Caddy read-only certificate access", command.String())
	}
	emit(progress, StageCertificate, StatusPending, "Install Certbot pre, deploy, and post hooks", "")
	emit(progress, StageCertificate, StatusPending, "Enable automatic certificate renewal", Command{Name: "systemctl", Args: []string{"enable", "--now", "certbot.timer"}}.String())
	emit(progress, StageConfig, StatusPending, "Write "+rootPath(opts.RootDir, "/etc/reverse/server.json")+" with mode 0600", "")
	emit(progress, StageContainer, StatusPending, "Build the reverse server image", dockerBuildCommand(opts).String())
	emit(progress, StageContainer, StatusPending, "Run the reverse server", dockerRunCommand(opts, rootPath(opts.RootDir, "/etc/reverse/server.json")).String())
	emit(progress, StageContainer, StatusPending, "Wait for the reverse server health endpoint", "")
	caddyPath := rootPath(opts.RootDir, "/etc/caddy/Caddyfile")
	emit(progress, StageConfig, StatusPending, "Write "+caddyPath, "")
	emit(progress, StageCaddy, StatusPending, "Validate Caddy", Command{Name: "caddy", Args: []string{"validate", "--config", caddyPath, "--adapter", "caddyfile"}}.String())
	emit(progress, StageCaddy, StatusPending, "Enable Caddy", Command{Name: "systemctl", Args: []string{"enable", "--now", "caddy"}}.String())
	emit(progress, StageCaddy, StatusPending, "Reload Caddy", Command{Name: "systemctl", Args: []string{"reload", "caddy"}}.String())
	emit(progress, StageComplete, StatusSuccess, "Dry-run plan is complete", "")
	return nil
}
