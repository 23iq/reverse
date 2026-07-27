package tunnel

import (
	"encoding/json"
	"net"
	"sync"
)

type controlWriter struct {
	conn   net.Conn
	events chan Event
	done   chan struct{}
	once   sync.Once
}

func newControlWriter(conn net.Conn, buffer int) *controlWriter {
	if buffer < 1 {
		buffer = 1
	}
	writer := &controlWriter{
		conn:   conn,
		events: make(chan Event, buffer),
		done:   make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (w *controlWriter) run() {
	encoder := json.NewEncoder(w.conn)
	for {
		select {
		case event := <-w.events:
			if err := encoder.Encode(event); err != nil {
				w.close()
				return
			}
		case <-w.done:
			return
		}
	}
}

func (w *controlWriter) emit(event Event) {
	select {
	case <-w.done:
		return
	default:
	}

	select {
	case w.events <- event:
	case <-w.done:
	default:
		// A slow dashboard must never stall proxied requests.
	}
}

func (w *controlWriter) close() {
	w.once.Do(func() {
		close(w.done)
		_ = w.conn.Close()
	})
}
