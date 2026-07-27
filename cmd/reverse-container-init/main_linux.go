//go:build linux

// reverse-container-init is the intentionally small privilege boundary used by
// the server image. It reads the root-only server configuration, permanently
// drops to the unprivileged runtime user, and preserves only the capability
// needed to reserve loopback ports below 1024.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	reverseBinary      = "/usr/local/bin/reverse"
	defaultConfigPath  = "/etc/reverse/server.json"
	runtimeID          = 65532
	maxServerConfigLen = 64 << 10
)

var bootstrapCapabilities = []int{
	unix.CAP_SETGID,
	unix.CAP_SETUID,
	unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE,
}

type configArgument struct {
	enabled bool
	index   int
	joined  bool
	source  string
}

func main() {
	runtime.LockOSThread()

	args := append([]string(nil), os.Args[1:]...)
	configArg, err := findConfigArgument(args)
	if err != nil {
		fatal(err)
	}

	var configData []byte
	if configArg.enabled {
		configData, err = readSecureConfig(configArg.source)
		if err != nil {
			fatal(err)
		}
	}

	if err := dropPrivileges(); err != nil {
		zero(configData)
		fatal(err)
	}

	var inheritedConfig *os.File
	if configArg.enabled {
		inheritedConfig, err = createSealedConfig(configData)
		zero(configData)
		if err != nil {
			fatal(err)
		}
		args = replaceConfigArgument(args, configArg, procFDPath(inheritedConfig.Fd()))
	}

	argv := append([]string{reverseBinary}, args...)
	err = unix.Exec(reverseBinary, argv, os.Environ())
	runtime.KeepAlive(inheritedConfig)
	if err != nil {
		fatal(fmt.Errorf("start reverse server: %w", err))
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "reverse container init: %v\n", err)
	os.Exit(1)
}

func findConfigArgument(args []string) (configArgument, error) {
	server := false
	config := configArgument{index: -1, source: defaultConfigPath}

	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "--server" || args[index] == "--server=true":
			server = true
		case args[index] == "--server=false":
			server = false
		case args[index] == "--server-config":
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
				return configArgument{}, errors.New("--server-config requires a path")
			}
			if config.index >= 0 {
				return configArgument{}, errors.New("--server-config may only be specified once")
			}
			config.index = index + 1
			config.source = args[index+1]
			index++
		case strings.HasPrefix(args[index], "--server-config="):
			if config.index >= 0 {
				return configArgument{}, errors.New("--server-config may only be specified once")
			}
			config.index = index
			config.joined = true
			config.source = strings.TrimPrefix(args[index], "--server-config=")
			if config.source == "" {
				return configArgument{}, errors.New("--server-config requires a path")
			}
		}
	}

	config.enabled = server
	return config, nil
}

func replaceConfigArgument(args []string, config configArgument, path string) []string {
	result := append([]string(nil), args...)
	if config.index < 0 {
		return append(result, "--server-config", path)
	}
	if config.joined {
		result[config.index] = "--server-config=" + path
	} else {
		result[config.index] = path
	}
	return result
}

func readSecureConfig(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open server config %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open server config: invalid file descriptor")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect server config %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("server config %q is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("server config %q must only be accessible by its owner (mode 0600 or stricter)", path)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxServerConfigLen+1))
	if err != nil {
		return nil, fmt.Errorf("read server config %q: %w", path, err)
	}
	if len(data) > maxServerConfigLen {
		zero(data)
		return nil, fmt.Errorf("server config %q exceeds %d bytes", path, maxServerConfigLen)
	}
	return data, nil
}

func createSealedConfig(data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("reverse-server-config", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create in-memory server config: %w", err)
	}
	file := os.NewFile(uintptr(fd), "reverse-server-config")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create in-memory server config: invalid file descriptor")
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()

	if err := file.Chmod(0o400); err != nil {
		return nil, fmt.Errorf("secure in-memory server config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return nil, fmt.Errorf("write in-memory server config: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind in-memory server config: %w", err)
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, fmt.Errorf("seal in-memory server config: %w", err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
		return nil, fmt.Errorf("inherit in-memory server config: %w", err)
	}

	ok = true
	return file, nil
}

func procFDPath(fd uintptr) string {
	return "/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10)
}

func dropPrivileges() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("expected root bootstrap user, got uid %d", os.Geteuid())
	}
	if err := verifyBootstrapCapabilities(); err != nil {
		return err
	}

	// Remove bootstrap-only capabilities from the bounding set before changing
	// identity. They remain effective just long enough to complete the drop.
	for _, capability := range []int{unix.CAP_SETGID, unix.CAP_SETUID, unix.CAP_SETPCAP} {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0); err != nil {
			return fmt.Errorf("drop bounding capability %s: %w", capabilityName(capability), err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("preserve capabilities while changing uid: %w", err)
	}
	if err := unix.Setgroups(nil); err != nil {
		return fmt.Errorf("clear supplementary groups: %w", err)
	}
	if err := unix.Setresgid(runtimeID, runtimeID, runtimeID); err != nil {
		return fmt.Errorf("drop group privileges: %w", err)
	}
	if err := unix.Setresuid(runtimeID, runtimeID, runtimeID); err != nil {
		return fmt.Errorf("drop user privileges: %w", err)
	}

	if err := setOnlyNetBindCapability(); err != nil {
		return err
	}
	if err := unix.Prctl(
		unix.PR_CAP_AMBIENT,
		unix.PR_CAP_AMBIENT_RAISE,
		unix.CAP_NET_BIND_SERVICE,
		0,
		0,
	); err != nil {
		return fmt.Errorf("preserve NET_BIND_SERVICE across exec: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("disable keep-caps mode: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable no-new-privileges: %w", err)
	}
	return verifyRuntimePrivileges()
}

func verifyBootstrapCapabilities() error {
	effective, permitted, inheritable, err := processCapabilities()
	if err != nil {
		return fmt.Errorf("inspect bootstrap capabilities: %w", err)
	}
	allowed := capabilityMask(bootstrapCapabilities...)
	required := allowed
	if effective != required || permitted != required || inheritable != 0 {
		return fmt.Errorf(
			"unsafe bootstrap capabilities (effective=%#x permitted=%#x inheritable=%#x); use the documented cap-drop/cap-add settings",
			effective,
			permitted,
			inheritable,
		)
	}
	bounding, err := boundingCapabilities()
	if err != nil {
		return fmt.Errorf("inspect bootstrap capability boundary: %w", err)
	}
	if bounding != allowed {
		return fmt.Errorf(
			"unsafe bootstrap capability boundary %#x; use the documented cap-drop/cap-add settings",
			bounding,
		)
	}
	return nil
}

func setOnlyNetBindCapability() error {
	mask := uint32(1) << uint(unix.CAP_NET_BIND_SERVICE)
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{{
		Effective:   mask,
		Permitted:   mask,
		Inheritable: mask,
	}}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("reduce runtime capabilities: %w", err)
	}
	return nil
}

func verifyRuntimePrivileges() error {
	if os.Getuid() != runtimeID || os.Geteuid() != runtimeID ||
		os.Getgid() != runtimeID || os.Getegid() != runtimeID {
		return fmt.Errorf(
			"privilege drop verification failed (uid=%d euid=%d gid=%d egid=%d)",
			os.Getuid(),
			os.Geteuid(),
			os.Getgid(),
			os.Getegid(),
		)
	}
	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("inspect supplementary groups: %w", err)
	}
	if len(groups) != 0 {
		return fmt.Errorf("privilege drop left supplementary groups %v", groups)
	}

	expected := capabilityMask(unix.CAP_NET_BIND_SERVICE)
	effective, permitted, inheritable, err := processCapabilities()
	if err != nil {
		return fmt.Errorf("inspect runtime capabilities: %w", err)
	}
	if effective != expected || permitted != expected || inheritable != expected {
		return fmt.Errorf(
			"runtime capability verification failed (effective=%#x permitted=%#x inheritable=%#x)",
			effective,
			permitted,
			inheritable,
		)
	}
	bounding, err := boundingCapabilities()
	if err != nil {
		return fmt.Errorf("inspect runtime capability boundary: %w", err)
	}
	if bounding != expected {
		return fmt.Errorf("runtime capability boundary verification failed: %#x", bounding)
	}
	for capability := 0; capability <= lastCapability(); capability++ {
		ambient, err := unix.PrctlRetInt(
			unix.PR_CAP_AMBIENT,
			unix.PR_CAP_AMBIENT_IS_SET,
			uintptr(capability),
			0,
			0,
		)
		if err != nil {
			return fmt.Errorf("inspect ambient capability %d: %w", capability, err)
		}
		want := 0
		if capability == unix.CAP_NET_BIND_SERVICE {
			want = 1
		}
		if ambient != want {
			return fmt.Errorf("unexpected ambient capability %s=%d", capabilityName(capability), ambient)
		}
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil {
		return fmt.Errorf("inspect no-new-privileges: %w", err)
	}
	if noNewPrivileges != 1 {
		return errors.New("no-new-privileges was not retained")
	}
	return nil
}

func processCapabilities() (effective, permitted, inheritable uint64, err error) {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return 0, 0, 0, err
	}
	return uint64(data[0].Effective) | uint64(data[1].Effective)<<32,
		uint64(data[0].Permitted) | uint64(data[1].Permitted)<<32,
		uint64(data[0].Inheritable) | uint64(data[1].Inheritable)<<32,
		nil
}

func boundingCapabilities() (uint64, error) {
	var result uint64
	for capability := 0; capability <= lastCapability(); capability++ {
		present, err := unix.PrctlRetInt(
			unix.PR_CAPBSET_READ,
			uintptr(capability),
			0,
			0,
			0,
		)
		if err != nil {
			return 0, err
		}
		if present == 1 {
			result |= capabilityMask(capability)
		}
	}
	return result, nil
}

func lastCapability() int {
	data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err == nil {
		if value, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
			return value
		}
	}
	return unix.CAP_LAST_CAP
}

func capabilityMask(capabilities ...int) uint64 {
	var result uint64
	for _, capability := range capabilities {
		result |= uint64(1) << uint(capability)
	}
	return result
}

func capabilityName(capability int) string {
	switch capability {
	case unix.CAP_SETGID:
		return "SETGID"
	case unix.CAP_SETUID:
		return "SETUID"
	case unix.CAP_SETPCAP:
		return "SETPCAP"
	case unix.CAP_NET_BIND_SERVICE:
		return "NET_BIND_SERVICE"
	default:
		return strconv.Itoa(capability)
	}
}

func zero(data []byte) {
	for index := range data {
		data[index] = 0
	}
}
