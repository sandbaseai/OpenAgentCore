package runtimegateway

import (
	"context"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"sync"
	"time"
)

// One fixed snapshot per connection, including closure while persistence is pending.
type heartbeatObservation struct {
	mu                       sync.Mutex
	started, finished        bool
	decode, set, store, hint *float64
}

func (o *heartbeatObservation) begin() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started || o.finished {
		return false
	}
	o.started = true
	return true
}
func (o *heartbeatObservation) record(stage string, at time.Time) {
	value := float64(time.Since(at)) / float64(time.Millisecond)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	switch stage {
	case "heartbeat_decode_ms":
		o.decode = &value
	case "capability_set_ms":
		o.set = &value
	case "heartbeat_store_ms":
		o.store = &value
	case "capability_hint_ms":
		o.hint = &value
	}
}
func (o *heartbeatObservation) finish(s *Session, outcome string) {
	o.mu.Lock()
	if !o.started || o.finished {
		o.mu.Unlock()
		return
	}
	o.finished = true
	fields := []any{"device_id", s.DeviceID, "connection_id", s.transportConnection, "heartbeat_decode_ms", boundaryLogDuration(o.decode), "capability_set_ms", boundaryLogDuration(o.set), "heartbeat_store_ms", boundaryLogDuration(o.store), "capability_hint_ms", boundaryLogDuration(o.hint), "outcome", outcome}
	o.mu.Unlock()
	obslog.Ctx(context.Background()).Info("runtime first heartbeat observed", fields...)
}

// Log scalar values: TextHandler formats non-nil pointers as addresses.
func boundaryLogDuration(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
