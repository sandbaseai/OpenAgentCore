package execution

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type closureBlockingHandler struct{ entered, release chan struct{} }

func (h *closureBlockingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *closureBlockingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "execution scheduling observation" {
		close(h.entered)
		<-h.release
	}
	return nil
}
func (h *closureBlockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *closureBlockingHandler) WithGroup(string) slog.Handler      { return h }
func TestClosureSlowLoggerOwnerLock(t *testing.T) {
	h := &closureBlockingHandler{make(chan struct{}), make(chan struct{})}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)
	var s schedulingObservations
	o := s.candidate("r1", "s1")
	done := make(chan struct{})
	go func() { s.finish(context.Background(), o, "completed"); close(done) }()
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("logger not reached")
	}
	ownerHeld := !s.mu.TryLock()
	if !ownerHeld {
		s.mu.Unlock()
	}
	scopeHeld := !o.mu.TryLock()
	if !scopeHeld {
		o.mu.Unlock()
	}
	candidateDone := make(chan struct{})
	go func() { s.candidate("r2", "s2"); close(candidateDone) }()
	select {
	case <-candidateDone:
		t.Log("candidate independent of logger")
	case <-time.After(40 * time.Millisecond):
		t.Log("unrelated candidate blocked behind logger")
	}
	close(h.release)
	<-done
	<-candidateDone
	if ownerHeld || scopeHeld {
		t.Fatalf("logger called while owner lock held=%v scope lock held=%v", ownerHeld, scopeHeld)
	}
}
