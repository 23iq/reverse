package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

const controlTimeout = 2 * time.Second

type controlRequest struct {
	Command    string `json:"command"`
	InstanceID string `json:"instance_id"`
}

type controlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State State  `json:"state"`
}

type ControlServer struct {
	paths      Paths
	instanceID string
	listener   net.Listener
	snapshot   func() State
	stop       func()
	closed     chan struct{}
	closeOnce  sync.Once
}

func ListenControl(paths Paths, instanceID string, snapshot func() State, stop func()) (*ControlServer, error) {
	if !Supported() {
		return nil, ErrUnsupported
	}
	if instanceID == "" || snapshot == nil || stop == nil {
		return nil, errors.New("invalid daemon control configuration")
	}
	if err := removeControl(paths); err != nil {
		return nil, err
	}
	listener, err := listenControl(paths.Control)
	if err != nil {
		return nil, fmt.Errorf("listen for daemon control: %w", err)
	}
	if err := os.Chmod(paths.Control, 0o600); err != nil {
		_ = listener.Close()
		_ = removeControl(paths)
		return nil, fmt.Errorf("secure daemon control socket: %w", err)
	}

	server := &ControlServer{
		paths:      paths,
		instanceID: instanceID,
		listener:   listener,
		snapshot:   snapshot,
		stop:       stop,
		closed:     make(chan struct{}),
	}
	go server.serve()
	return server, nil
}

func (s *ControlServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.listener.Close()
		<-s.closed
		if removeErr := removeControl(s.paths); err == nil {
			err = removeErr
		}
	})
	return err
}

func (s *ControlServer) serve() {
	defer close(s.closed)
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(connection)
	}
}

func (s *ControlServer) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(controlTimeout))

	var request controlRequest
	if err := json.NewDecoder(connection).Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(controlResponse{Error: "invalid control request"})
		return
	}
	if request.InstanceID != s.instanceID {
		_ = json.NewEncoder(connection).Encode(controlResponse{Error: "daemon instance changed"})
		return
	}

	response := controlResponse{OK: true, State: s.snapshot()}
	switch request.Command {
	case "status":
		_ = json.NewEncoder(connection).Encode(response)
	case "stop":
		if err := json.NewEncoder(connection).Encode(response); err == nil {
			s.stop()
		}
	default:
		_ = json.NewEncoder(connection).Encode(controlResponse{Error: "unknown control command"})
	}
}

func Query(paths Paths) (State, error) {
	return request(paths, "status")
}

func Stop(paths Paths) (State, error) {
	return request(paths, "stop")
}

// CleanupStale removes only replaceable runtime metadata. The lock file is
// intentionally retained because a live worker may still hold its inode.
func CleanupStale(paths Paths) error {
	if err := removeControl(paths); err != nil {
		return err
	}
	return RemoveState(paths)
}

func request(paths Paths, command string) (State, error) {
	stored, err := ReadState(paths)
	if err != nil {
		return State{}, err
	}
	connection, err := dialControl(paths.Control, controlTimeout)
	if err != nil {
		return stored, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(controlTimeout))

	if err := json.NewEncoder(connection).Encode(controlRequest{
		Command:    command,
		InstanceID: stored.InstanceID,
	}); err != nil {
		return stored, fmt.Errorf("%w: send control request: %v", ErrNotRunning, err)
	}
	var response controlResponse
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return stored, fmt.Errorf("%w: read control response: %v", ErrNotRunning, err)
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "control request rejected"
		}
		return stored, fmt.Errorf("%w: %s", ErrNotRunning, response.Error)
	}
	if response.State.InstanceID != stored.InstanceID || response.State.PID != stored.PID {
		return stored, fmt.Errorf("%w: daemon identity changed", ErrNotRunning)
	}
	return response.State, nil
}

func removeControl(paths Paths) error {
	err := os.Remove(paths.Control)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale daemon control socket: %w", err)
	}
	return nil
}
