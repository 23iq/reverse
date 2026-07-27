//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFindConfigArgument(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    configArgument
		wantErr string
	}{
		{
			name: "separate",
			args: []string{"--server", "--server-config", "/config/server.json"},
			want: configArgument{
				enabled: true,
				index:   2,
				source:  "/config/server.json",
			},
		},
		{
			name: "joined",
			args: []string{"--server-config=/config/server.json", "--server"},
			want: configArgument{
				enabled: true,
				index:   0,
				joined:  true,
				source:  "/config/server.json",
			},
		},
		{
			name: "default",
			args: []string{"--server"},
			want: configArgument{
				enabled: true,
				index:   -1,
				source:  defaultConfigPath,
			},
		},
		{
			name: "client command does not open server config",
			args: []string{"--help"},
			want: configArgument{
				index:  -1,
				source: defaultConfigPath,
			},
		},
		{
			name:    "missing path",
			args:    []string{"--server", "--server-config"},
			wantErr: "requires a path",
		},
		{
			name:    "duplicate",
			args:    []string{"--server", "--server-config", "/a", "--server-config=/b"},
			wantErr: "only be specified once",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := findConfigArgument(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("findConfigArgument() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("findConfigArgument() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("findConfigArgument() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestReplaceConfigArgument(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		config configArgument
		want   []string
	}{
		{
			name:   "append default",
			args:   []string{"--server"},
			config: configArgument{enabled: true, index: -1},
			want:   []string{"--server", "--server-config", "/proc/self/fd/3"},
		},
		{
			name:   "replace separate",
			args:   []string{"--server", "--server-config", "/old"},
			config: configArgument{enabled: true, index: 2},
			want:   []string{"--server", "--server-config", "/proc/self/fd/3"},
		},
		{
			name:   "replace joined",
			args:   []string{"--server", "--server-config=/old"},
			config: configArgument{enabled: true, index: 1, joined: true},
			want:   []string{"--server", "--server-config=/proc/self/fd/3"},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			original := append([]string(nil), test.args...)
			got := replaceConfigArgument(test.args, test.config, "/proc/self/fd/3")
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("replaceConfigArgument() = %#v, want %#v", got, test.want)
			}
			if !reflect.DeepEqual(test.args, original) {
				t.Fatalf("replaceConfigArgument() mutated input: %#v", test.args)
			}
		})
	}
}

func TestReadSecureConfig(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	validPath := filepath.Join(directory, "server.json")
	if err := os.WriteFile(validPath, []byte(`{"domain":"tunnel.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readSecureConfig(validPath)
	if err != nil {
		t.Fatalf("readSecureConfig() error = %v", err)
	}
	if string(data) != `{"domain":"tunnel.example.com"}` {
		t.Fatalf("readSecureConfig() = %q", data)
	}

	writablePath := filepath.Join(directory, "writable.json")
	if err := os.WriteFile(writablePath, []byte("{}"), 0o622); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writablePath, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureConfig(writablePath); err == nil ||
		!strings.Contains(err.Error(), "only be accessible by its owner") {
		t.Fatalf("readSecureConfig(insecure mode) error = %v", err)
	}

	symlinkPath := filepath.Join(directory, "server-link.json")
	if err := os.Symlink(validPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureConfig(symlinkPath); err == nil {
		t.Fatal("readSecureConfig(symlink) unexpectedly succeeded")
	}
}

func TestReadSecureConfigRejectsOversizeFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxServerConfigLen+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureConfig(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readSecureConfig() error = %v", err)
	}
}

func TestCreateSealedConfig(t *testing.T) {
	t.Parallel()

	want := []byte(`{"password_hash":"test"}`)
	file, err := createSealedConfig(want)
	if errors.Is(err, unix.ENOSYS) {
		t.Skip("memfd is unavailable on this kernel")
	}
	if err != nil {
		t.Fatalf("createSealedConfig() error = %v", err)
	}
	defer file.Close()

	got, err := os.ReadFile(procFDPath(file.Fd()))
	if err != nil {
		t.Fatalf("read sealed config: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sealed config = %q, want %q", got, want)
	}
	if _, err := file.WriteAt([]byte("x"), 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("write sealed config error = %v, want EPERM", err)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("inspect descriptor flags: %v", err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("sealed config descriptor has close-on-exec set")
	}
}

func TestPrivilegeDropAllowsLowPort(t *testing.T) {
	if os.Getenv("REVERSE_RUN_PRIVILEGE_INTEGRATION") != "1" {
		t.Skip("requires the documented container capability set")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := dropPrivileges(); err != nil {
		t.Fatalf("dropPrivileges() error = %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:81")
	if err != nil {
		t.Fatalf("listen on privileged loopback port: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close privileged loopback listener: %v", err)
	}
}
