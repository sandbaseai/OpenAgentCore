package pgunit

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

type samplingDelayConn struct {
	net.Conn
	armed    *atomic.Bool
	entered  chan struct{}
	release  <-chan struct{}
	deadline chan struct{}
	signal   *sync.Once
}

func (c *samplingDelayConn) Read(p []byte) (int, error) {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
	}
	return c.Conn.Read(p)
}

func (c *samplingDelayConn) SetDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now().Add(50*time.Millisecond)) {
		c.signal.Do(func() { close(c.deadline) })
	}
	return c.Conn.SetDeadline(t)
}

// A short observational context can destroy the real writer's connection even
// though Ping writes no application data. Samplers must never pass such contexts
// to the lease; only the execution owner performs authoritative checks.
func TestCancelledOwnershipPingClosesExecutionLease(t *testing.T) {
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	deadline := make(chan struct{})
	var signal, unblock sync.Once
	releaseRead := func() { unblock.Do(func() { close(release) }) }
	t.Cleanup(releaseRead)
	pool := pgtest.OpenIsolated(t, func(cfg *pgxpool.Config) {
		dial := cfg.ConnConfig.DialFunc
		cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &samplingDelayConn{Conn: conn, armed: &armed, entered: entered, release: release, deadline: deadline, signal: &signal}, nil
		}
	})
	lease := acquireLease(t, pool)
	armed.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lease.CheckOwnership(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ownership Ping did not read")
	}
	cancel()
	select {
	case <-deadline:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach the driver deadline")
	}
	releaseRead()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !lease.conn.Conn().PgConn().IsClosed() {
			t.Fatal("cancelled Ping did not invalidate lease", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Ping did not settle")
	}
	if lease.CheckOwnership(t.Context()) == nil {
		t.Fatal("cancelled observational Ping retained writer ownership")
	}
}
