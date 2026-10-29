package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/transport"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto/prototest"
	"github.com/MiniMax-AI/OpenAgentCore/internal/harnessconfig"
	"github.com/MiniMax-AI/OpenAgentCore/internal/machineapi"
	"github.com/gorilla/websocket"
)

func TestReconnectWaitsForConfirmedRouterCleanup(t *testing.T) {
	for _, failure := range []error{errors.New("native close failed"), context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			var calls atomic.Int32
			retry := make(chan struct{})
			confirm := make(chan struct{})
			returned := make(chan struct{})
			go func() {
				shutdownRouterUntilConfirmed(func(ctx context.Context) error {
					if ctx.Err() != nil {
						t.Error("cleanup inherited a cancelled connection context")
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Error("cleanup wait has no deadline")
					}
					if calls.Add(1) == 1 {
						return failure
					}
					close(retry)
					<-confirm
					return nil
				}, time.Millisecond)
				close(returned)
			}()
			select {
			case <-retry:
			case <-time.After(time.Second):
				t.Fatal("same cleanup was not retried")
			}
			select {
			case <-returned:
				t.Fatal("reconnect released unconfirmed ownership")
			default:
			}
			close(confirm)
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("confirmed cleanup did not release reconnect")
			}
			if calls.Load() != 2 {
				t.Fatalf("cleanup attempts=%d", calls.Load())
			}
		})
	}
}

type cleanupExecutor struct {
	started   chan struct{}
	cancelled chan struct{}
	closes    atomic.Int32
	retry     chan struct{}
	confirm   chan struct{}
}

func (e *cleanupExecutor) StartTurn(_ context.Context, _ string, _ proto.MessageInput, out chan<- proto.Envelope) (agent.Turn, error) {
	if e.started == nil {
		return nil, errors.New("unexpected Turn")
	}
	close(e.started)
	return &cleanupActiveTurn{out: out, cancelled: e.cancelled}, nil
}

type cleanupActiveTurn struct {
	agent.Turn
	out       chan<- proto.Envelope
	cancelled chan struct{}
	once      sync.Once
}

func (t *cleanupActiveTurn) Cancel(context.Context) error {
	t.once.Do(func() { close(t.cancelled); close(t.out) })
	return nil
}
func (t *cleanupActiveTurn) CancellationOutcome() proto.DonePayload { return proto.DonePayload{} }
func (t *cleanupActiveTurn) AwaitSettlement(ctx context.Context) (agent.TurnSettlement, error) {
	select {
	case <-t.cancelled:
		return agent.TurnSettlement{Reusable: true}, nil
	case <-ctx.Done():
		return agent.TurnSettlement{}, ctx.Err()
	}
}
func (e *cleanupExecutor) Close(context.Context) error {
	if e.closes.Add(1) == 1 {
		return errors.New("native cleanup temporarily unavailable")
	}
	close(e.retry)
	<-e.confirm
	return nil
}

func TestDisconnectedPumpRetainsExactExecutorUntilCleanup(t *testing.T) {
	for _, suspend := range []bool{false, true} {
		name := "ordinary"
		if suspend {
			name = "suspension-enabled"
		}
		t.Run(name, func(t *testing.T) { testDisconnectedPumpCleanup(t, suspend, false) })
	}
}

func TestSilentConnectionStopsOwnerBeforeReconnect(t *testing.T) {
	testDisconnectedPumpCleanup(t, true, true)
}

func testDisconnectedPumpCleanup(t *testing.T, suspend, silent bool) {
	peers := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			peers <- peer
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dial := func(ctx context.Context) (*transport.Conn, error) {
		return transport.Dial(ctx, transport.DialOptions{
			WSURL: "ws" + strings.TrimPrefix(server.URL, "http"), DeviceID: "device",
			Credential: "credential", DaemonVersion: proto.Version,
		})
	}
	owner := &cleanupExecutor{retry: make(chan struct{}), confirm: make(chan struct{})}
	if silent {
		owner.started = make(chan struct{})
		owner.cancelled = make(chan struct{})
	}
	registry := agent.NewRegistry()
	registry.RegisterKind(proto.SupportedAgentKind{Kind: "cleanup", Available: true, Capabilities: prototest.Capabilities(proto.AgentKindCapabilities{EnvironmentNone: proto.CapabilitySupported})}, harnessconfig.Configuration{})
	var factories atomic.Int32
	registry.RegisterExecutor("cleanup", func(context.Context, proto.PromptRequestPayload) (agent.Executor, error) {
		factories.Add(1)
		return owner, nil
	})
	finished := make(chan error, 1)
	go func() {
		boot := &machineapi.BootstrapResponse{HeartbeatSeconds: 60}
		if suspend {
			control := &suspendControl{signal: make(chan os.Signal, 1)}
			finished <- runSuspendLoop(ctx, dial, registry, boot, agentCLIDiscovery{}, control)
			return
		}
		conn, err := dial(ctx)
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		finished <- pumpConn(ctx, conn, registry, boot, agentCLIDiscovery{})
	}()
	var peer *websocket.Conn
	select {
	case peer = <-peers:
	case <-time.After(3 * time.Second):
		t.Fatal("initial connection missing")
	}
	defer peer.Close()
	env, err := proto.NewEnvelope(proto.TypeExecutionPrepare, "prepare", proto.ExecutionPreparePayload{SessionID: "cleanup",
		Configuration: proto.PromptRequestPayload{AgentKind: "cleanup", AgentStateKey: proto.SessionStateKey("cleanup"), DisableExecutionEnvironment: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.WriteJSON(env); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var result proto.Envelope
		if err := peer.ReadJSON(&result); err != nil {
			t.Fatal(err)
		}
		if result.Type != proto.TypePreparationStatus {
			continue
		}
		var status proto.PreparationStatusPayload
		if err := result.DecodePayload(&status); err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" {
			if silent {
				start, _ := proto.NewEnvelope(proto.TypeExecutionStart, "prepare", proto.ExecutionStartPayload{Handle: status.Handle, ExecutorID: status.ExecutorID, RunID: "active-run", Input: proto.TextInput("controlled fixture")})
				if err := peer.WriteJSON(start); err != nil {
					t.Fatal(err)
				}
				select {
				case <-owner.started:
				case <-time.After(3 * time.Second):
					t.Fatal("active Turn did not start")
				}
			}
			break
		}
		if status.State == "failed" {
			t.Fatalf("preparation failed: %+v", status)
		}
	}
	wait := 3 * time.Second
	if silent {
		wait = 70 * time.Second
	} else {
		_ = peer.Close()
	}
	select {
	case <-owner.retry:
		if silent {
			select {
			case <-owner.cancelled:
			default:
				t.Fatal("owner cleanup did not cancel active Turn")
			}
		}
	case <-time.After(wait):
		t.Fatal("original executor cleanup was not retried")
	}
	select {
	case err := <-finished:
		t.Fatalf("pump discarded native ownership: %v", err)
	default:
	}
	cancel()
	close(owner.confirm)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("pump did not return after confirmed cleanup")
	}
	if factories.Load() != 1 || owner.closes.Load() != 2 {
		t.Fatalf("factory=%d close=%d", factories.Load(), owner.closes.Load())
	}
}
