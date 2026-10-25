package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
)

type captureBudgetStorage struct {
	sessions.ExecutionStorage
	entered chan context.Context
}

func (s captureBudgetStorage) WithTurns(ctx context.Context, _, _ string, _ func(context.Context, sessions.TurnTx) error) error {
	s.entered <- ctx
	<-ctx.Done()
	return ctx.Err()
}

type captureBudgetReader struct{ sessions.Reader }

func (captureBudgetReader) GetTurn(context.Context, string, string, string) (sessions.Turn, error) {
	return sessions.Turn{}, sessions.ErrNotFound
}

func TestArtifactCaptureTenMinuteBudgetKeepsFailureSemantics(t *testing.T) {
	for _, budget := range []time.Duration{10 * time.Minute, time.Minute} {
		t.Run(budget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan context.Context, 1)
				operations, err := sessions.NewExecutionOperations(captureBudgetStorage{entered: entered})
				if err != nil {
					t.Fatal(err)
				}
				d := &Dispatcher{sessionExecution: operations, SessionsReader: captureBudgetReader{}}
				parent, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				done := make(chan struct{})
				var result Result
				var status string
				go func() {
					result, status = d.captureCompletedArtifacts(parent, nil, sessions.Session{TenantID: uuid.NewString(), ID: uuid.NewString()}, sessions.Environment{Configuration: []byte(`{"type":"openai_hosted"}`)}, sessions.ExecutionDevice{}, uuid.NewString(), Result{}, sessions.TurnCompleted)
					close(done)
				}()
				owner := <-entered
				deadline, ok := owner.Deadline()
				if !ok || deadline.Sub(time.Now()) != budget {
					t.Fatalf("capture deadline=%v, want %v", deadline, budget)
				}
				time.Sleep(budget - time.Second)
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("capture stopped early")
				default:
				}
				time.Sleep(time.Second)
				<-done
				if status != sessions.TurnFailed || result.ErrorCode != "artifact_capture_failed" {
					t.Fatalf("capture failure=%s %+v", status, result)
				}
			})
		})
	}
}

func TestReadOnlyReleaseAcceptsExpiredCleanupAfterLongExport(t *testing.T) {
	for _, test := range []struct {
		name, state, handle, run, code string
		wantOK                         bool
	}{
		{"expired", "expired", "prepared", "", "", true},
		{"released", "released", "prepared", "", "", true},
		{"wrong_handle", "expired", "foreign", "", "", false},
		{"native_run", "expired", "prepared", "native", "", false},
		{"cleanup_error", "expired", "prepared", "", "cleanup_failed", false},
		{"failed", "failed", "prepared", "", "", false},
		{"unknown", "unknown", "prepared", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// No transport goroutines are needed: Send queues the release and the
				// correlated typed terminal receipt is already available to the caller.
				peer := runtimegateway.NewSession(nil, "device", "test", nil, nil)
				replies := make(chan proto.Envelope, 1)
				env, err := proto.NewEnvelope(proto.TypePreparationStatus, "prepare", proto.PreparationStatusPayload{Handle: test.handle, Revision: 3, State: test.state, RunID: test.run, ErrorCode: test.code})
				if err != nil {
					t.Fatal(err)
				}
				replies <- env
				p := &preparedStart{peer: peer, requestID: "prepare", handle: "prepared", sub: &runtimegateway.Subscription{Events: replies}}
				time.Sleep(6 * time.Minute)
				if err := p.releaseDirectory(); (err == nil) != test.wantOK {
					t.Fatalf("release=%v, wantOK=%v", err, test.wantOK)
				}
			})
		})
	}
}

type expiredPreparationConn struct {
	inbound chan []byte
	closed  chan struct{}
	once    sync.Once
}

func (c *expiredPreparationConn) ReadMessage() (int, []byte, error) {
	select {
	case body := <-c.inbound:
		return 1, body, nil
	case <-c.closed:
		return 0, nil, io.EOF
	}
}
func (c *expiredPreparationConn) WriteMessage(_ int, body []byte) error {
	var request proto.Envelope
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	state, revision := "ready", uint64(1)
	if request.Type == proto.TypeExecutionRelease {
		state, revision = "expired", 2
	}
	response, _ := proto.NewEnvelope(proto.TypePreparationStatus, request.ID, proto.PreparationStatusPayload{Handle: "prepared", Revision: revision, State: state})
	raw, _ := json.Marshal(response)
	select {
	case c.inbound <- raw:
		return nil
	case <-c.closed:
		return io.EOF
	}
}
func (*expiredPreparationConn) WriteControl(int, []byte, time.Time) error { return nil }
func (*expiredPreparationConn) SetReadLimit(int64)                        {}
func (*expiredPreparationConn) SetReadDeadline(time.Time) error           { return nil }
func (*expiredPreparationConn) SetWriteDeadline(time.Time) error          { return nil }
func (c *expiredPreparationConn) Close() error                            { c.once.Do(func() { close(c.closed) }); return nil }

func TestPreparedWorkspaceRetainsConsumeResultAfterExpiredCleanup(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn := &expiredPreparationConn{inbound: make(chan []byte, 4), closed: make(chan struct{})}
				peer := runtimegateway.NewSession(conn, "device", "test", runtimegateway.NewRegistry(), func(string, ...any) {})
				peer.Start()
				defer peer.Close("test")
				sentinel := errors.New("capture failed")
				var consumeErr error
				if failed {
					consumeErr = sentinel
				}
				owner, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				err := (&Dispatcher{}).withPreparedWorkspace(owner, peer, sessions.Session{ID: "session", TenantID: "tenant"}, sessions.Environment{ID: "environment", SessionID: "session", TenantID: "tenant", Configuration: []byte(`{"type":"openai_hosted"}`)}, sessions.ExecutionDevice{EnvironmentID: "environment"}, func(context.Context, string) error {
					time.Sleep(6 * time.Minute)
					return consumeErr
				})
				if err != consumeErr {
					t.Fatalf("consume error=%v, got=%v", consumeErr, err)
				}
			})
		})
	}
}
