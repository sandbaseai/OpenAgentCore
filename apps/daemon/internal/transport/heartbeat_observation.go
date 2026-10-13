package transport

import (
	"log/slog"
	"sync"
	"time"
)

// First-attempt timing only; local send completion is not remote receipt.
type heartbeatObservation struct {
	mu                sync.Mutex
	started, finished bool
	build, send       *float64
	logger            *slog.Logger
}

func (o *heartbeatObservation) begin(logger *slog.Logger) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started || o.finished {
		return false
	}
	o.started = true
	o.logger = logger
	return true
}
func (o *heartbeatObservation) record(build bool, at time.Time) {
	value := float64(time.Since(at)) / float64(time.Millisecond)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	if build {
		o.build = &value
	} else {
		o.send = &value
	}
}
func (o *heartbeatObservation) finish(c *Conn, outcome string) {
	o.mu.Lock()
	if !o.started || o.finished {
		o.mu.Unlock()
		return
	}
	o.finished = true
	fields := []any{"device_id", c.deviceID, "connection_id", c.connectionID, "capability_payload_build_ms", boundaryLogDuration(o.build), "first_heartbeat_send_ms", boundaryLogDuration(o.send), "outcome", outcome}
	logger := o.logger
	o.mu.Unlock()
	logger.Info("runtime first heartbeat", fields...)
}

// Log scalar values: TextHandler formats non-nil pointers as addresses.
func boundaryLogDuration(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
