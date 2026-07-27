package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	stateSchemaVersion = 1
	maxStateSize       = 1 << 20
	stateWriteDelay    = 200 * time.Millisecond
	stateReadAttempts  = 3
)

var (
	ErrAlreadyRunning = errors.New("a background reverse tunnel is already running")
	ErrNotRunning     = errors.New("no background reverse tunnel is running")
	ErrUnsupported    = errors.New("background mode is not supported on this platform")
	errStateReplaced  = errors.New("daemon state changed while it was opened")
)

// State contains operational metadata only. Authentication tokens and client
// configuration are deliberately never copied into the runtime state.
type State struct {
	SchemaVersion int       `json:"schema_version"`
	InstanceID    string    `json:"instance_id"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	PublicURL     string    `json:"public_url"`
	LocalTarget   string    `json:"local_target"`
	Status        string    `json:"status"`
	Attempt       int       `json:"attempt,omitempty"`
	Requests      uint64    `json:"requests"`
	BytesIn       uint64    `json:"bytes_in"`
	BytesOut      uint64    `json:"bytes_out"`
	LastEventAt   time.Time `json:"last_event_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

type Paths struct {
	Dir     string
	State   string
	Lock    string
	Control string
}

// RuntimePaths returns a private per-user runtime directory. The override is
// useful for isolated service environments and tests.
func RuntimePaths() (Paths, error) {
	var dir string
	if override := os.Getenv("REVERSE_RUNTIME_DIR"); override != "" {
		if !filepath.IsAbs(override) {
			return Paths{}, errors.New("REVERSE_RUNTIME_DIR must be an absolute path")
		}
		dir = filepath.Clean(override)
	} else if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		if !filepath.IsAbs(runtimeDir) {
			return Paths{}, errors.New("XDG_RUNTIME_DIR must be an absolute path")
		}
		dir = filepath.Join(filepath.Clean(runtimeDir), "reverse")
	} else {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return Paths{}, fmt.Errorf("find user cache directory: %w", err)
		}
		dir = filepath.Join(cacheDir, "reverse", "run")
	}

	if err := ensurePrivateDir(dir); err != nil {
		return Paths{}, err
	}
	return Paths{
		Dir:     dir,
		State:   filepath.Join(dir, "daemon.json"),
		Lock:    filepath.Join(dir, "daemon.lock"),
		Control: filepath.Join(dir, "control.sock"),
	}, nil
}

func ensurePrivateDir(path string) error {
	created := false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create reverse runtime directory: %w", err)
		}
		created = true
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect reverse runtime directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("reverse runtime path is not a private directory")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("reverse runtime directory is owned by another user")
	}
	if created {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure reverse runtime directory: %w", err)
		}
	} else if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("reverse runtime directory permissions are %04o, want 0700", info.Mode().Perm())
	}
	return nil
}

func NewInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate daemon instance identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func ReadState(paths Paths) (State, error) {
	var replacedErr error
	for attempt := 0; attempt < stateReadAttempts; attempt++ {
		state, err := readStateOnce(paths)
		if !errors.Is(err, errStateReplaced) {
			return state, err
		}
		replacedErr = err
	}
	return State{}, replacedErr
}

func readStateOnce(paths Paths) (State, error) {
	expected, err := os.Lstat(paths.State)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, ErrNotRunning
		}
		return State{}, fmt.Errorf("inspect daemon state path: %w", err)
	}
	if !expected.Mode().IsRegular() {
		return State{}, errors.New("daemon state is not a regular file")
	}
	if !ownedByCurrentUser(expected) {
		return State{}, errors.New("daemon state is owned by another user")
	}
	if expected.Mode().Perm() != 0o600 {
		return State{}, fmt.Errorf("daemon state permissions are %04o, want 0600", expected.Mode().Perm())
	}

	file, err := os.Open(paths.State)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, ErrNotRunning
		}
		return State{}, fmt.Errorf("open daemon state: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return State{}, fmt.Errorf("inspect daemon state: %w", err)
	}
	if !os.SameFile(expected, info) || !info.Mode().IsRegular() {
		return State{}, errStateReplaced
	}
	if info.Size() > maxStateSize {
		return State{}, errors.New("daemon state is too large")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxStateSize+1))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode daemon state: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return State{}, errors.New("daemon state contains trailing JSON")
		}
		return State{}, fmt.Errorf("decode daemon state trailer: %w", err)
	}
	if state.SchemaVersion != stateSchemaVersion || state.InstanceID == "" || state.PID < 1 {
		return State{}, errors.New("daemon state is invalid")
	}
	return state, nil
}

func WriteState(paths Paths, state State) error {
	state.SchemaVersion = stateSchemaVersion
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}
	temporary, err := os.CreateTemp(paths.Dir, ".daemon-*.tmp")
	if err != nil {
		return fmt.Errorf("create daemon state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure daemon state: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode daemon state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync daemon state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close daemon state: %w", err)
	}
	if err := os.Rename(temporaryPath, paths.State); err != nil {
		return fmt.Errorf("publish daemon state: %w", err)
	}
	if err := os.Chmod(paths.State, 0o600); err != nil {
		return fmt.Errorf("secure published daemon state: %w", err)
	}
	return nil
}

func RemoveState(paths Paths) error {
	err := os.Remove(paths.State)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon state: %w", err)
	}
	return nil
}

type Store struct {
	paths Paths

	mu      sync.RWMutex
	state   State
	closed  bool
	lastErr error

	wake     chan struct{}
	commands chan storeCommand
	done     chan struct{}
}

type storeCommand struct {
	close bool
	reply chan error
}

func NewStore(paths Paths, initial State) (*Store, error) {
	initial.SchemaVersion = stateSchemaVersion
	if initial.UpdatedAt.IsZero() {
		initial.UpdatedAt = time.Now().UTC()
	}
	if err := WriteState(paths, initial); err != nil {
		return nil, err
	}
	store := &Store{
		paths:    paths,
		state:    initial,
		wake:     make(chan struct{}, 1),
		commands: make(chan storeCommand),
		done:     make(chan struct{}),
	}
	go store.run()
	return store, nil
}

func (s *Store) Update(update func(*State)) {
	if update == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	update(&s.state)
	s.state.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Store) Flush() error {
	reply := make(chan error, 1)
	select {
	case s.commands <- storeCommand{reply: reply}:
	case <-s.done:
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.lastErr
	}
	return <-reply
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		return s.LastError()
	}
	s.closed = true
	s.mu.Unlock()

	reply := make(chan error, 1)
	s.commands <- storeCommand{close: true, reply: reply}
	err := <-reply
	<-s.done
	return err
}

func (s *Store) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

func (s *Store) run() {
	defer close(s.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var timerChannel <-chan time.Time
	dirty := false

	write := func() error {
		s.mu.RLock()
		snapshot := s.state
		s.mu.RUnlock()
		err := WriteState(s.paths, snapshot)
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		dirty = false
		return err
	}

	for {
		select {
		case <-s.wake:
			dirty = true
			if timerChannel == nil {
				timer.Reset(stateWriteDelay)
				timerChannel = timer.C
			}
		case <-timerChannel:
			timerChannel = nil
			if dirty {
				_ = write()
			}
		case command := <-s.commands:
			if timerChannel != nil && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerChannel = nil
			// Always write here: an Update may have acquired the mutex before
			// its wake notification was selected by this loop.
			err := write()
			command.reply <- err
			if command.close {
				return
			}
		}
	}
}
