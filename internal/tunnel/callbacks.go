package tunnel

import (
	"sync/atomic"
	"time"
)

func callEvent(callback EventCallback, event Event) {
	if callback == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	callback(event)
}

func callStatus(callback StatusCallback, status Status) {
	if callback == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	callback(status)
}

type trafficMeter struct {
	toLocal   atomic.Uint64
	fromLocal atomic.Uint64
	callback  TrafficCallback
}

func (m *trafficMeter) add(direction TrafficDirection, bytes int64) {
	if bytes <= 0 {
		return
	}

	switch direction {
	case TrafficToLocal:
		m.toLocal.Add(uint64(bytes))
	case TrafficFromLocal:
		m.fromLocal.Add(uint64(bytes))
	default:
		return
	}

	if m.callback == nil {
		return
	}
	report := Traffic{
		At:             time.Now().UTC(),
		Direction:      direction,
		Bytes:          bytes,
		TotalToLocal:   m.toLocal.Load(),
		TotalFromLocal: m.fromLocal.Load(),
	}
	defer func() {
		_ = recover()
	}()
	m.callback(report)
}
