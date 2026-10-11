package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto/prototest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/deployment"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/environmentconfig"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimedevice"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/gorilla/websocket"
)

type initializationWakeReader struct {
	sessions.Reader
	committed bool
	scans     int
}

func (*initializationWakeReader) GetEnvironment(context.Context, string, string) (sessions.Environment, error) {
	return sessions.Environment{ID: "environment", Configuration: json.RawMessage(`{"type":"self_hosted","workspace_directory":"/workspace"}`)}, nil
}
func (*initializationWakeReader) ReadEnvironmentSetup(context.Context, string, string) (environmentconfig.Setup, error) {
	return environmentconfig.Setup{}, nil
}
func (*initializationWakeReader) GetDeviceCredential(context.Context, string) (runtimedevice.Credential, bool, error) {
	return runtimedevice.Credential{ID: "device", Type: runtimedevice.RuntimeTypeAgentDaemon, CredentialHash: runtimedevice.HashCredential("fixture")}, true, nil
}
func (*initializationWakeReader) ListExecutionWork(context.Context, string, []string, []string) ([]sessions.ExecutionWork, error) {
	return nil, nil
}
func (r *initializationWakeReader) ListEnvironmentInputWork(context.Context, string, []string) ([]sessions.EnvironmentInputWork, error) {
	r.scans++
	if !r.committed {
		return nil, nil
	}
	return []sessions.EnvironmentInputWork{{TenantID: "tenant", SessionID: "session", ReservationID: "reservation"}}, nil
}

func (r *initializationWakeReader) GetSession(context.Context, string, string) (sessions.Session, error) {
	return sessions.Session{ID: "session", TenantID: "tenant", Engine: "codex", Configuration: json.RawMessage(`{"agent":{"model":"fixture"},"environment":{"type":"self_hosted","workspace_directory":"/workspace"}}`)}, nil
}
func (r *initializationWakeReader) GetSessionEnvironment(ctx context.Context, _, _ string) (sessions.Environment, error) {
	e, _ := r.GetEnvironment(ctx, "", "")
	e.SessionID = "session"
	e.TenantID = "tenant"
	return e, nil
}
func (*initializationWakeReader) GetSessionDevice(context.Context, string, string) (sessions.ExecutionDevice, error) {
	return sessions.ExecutionDevice{ID: "device", EnvironmentID: "environment"}, nil
}

type initializationWakeTx struct{ sessions.InitializationTx }

func (*initializationWakeTx) LoadEnvironment(context.Context) (sessions.Environment, error) {
	return sessions.Environment{ID: "environment"}, nil
}
func (*initializationWakeTx) LoadSessionDevice(context.Context) (sessions.ExecutionDevice, bool, error) {
	return sessions.ExecutionDevice{ID: "device", EnvironmentID: "environment"}, true, nil
}
func (*initializationWakeTx) CompleteInitialization(context.Context, string) (bool, error) {
	return true, nil
}

type initializationWakeStorage struct {
	sessions.ExecutionStorage
	reader      *initializationWakeReader
	worker      *Worker
	calls       int
	commitError error
}

func (s *initializationWakeStorage) WithInitialization(ctx context.Context, _, _ string, apply func(context.Context, sessions.InitializationTx, sessions.LockedSession) error) error {
	s.calls++
	if s.calls > 1 {
		return s.commitError
	} // The failure recorder must not fabricate readiness.
	if err := apply(ctx, &initializationWakeTx{}, sessions.LockedSession{}); err != nil {
		return err
	}
	if len(s.worker.scheduleWake) != 0 {
		return errors.New("scheduler woke before initialization commit")
	}
	if s.commitError != nil {
		return s.commitError
	}
	s.reader.committed = true
	return nil
}

// The initial capability hint can be consumed while initialization is pending.
// A committed initialization must trigger another actual candidate scan without
// waiting for the one-second Environment scan deadline; a failed commit must not.
func TestInitializationCommitWakesThrottledScheduler(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "commit_failed"}[failed], func(t *testing.T) {
			reader := &initializationWakeReader{}
			registry := runtimegateway.NewRegistry()
			handler := runtimegateway.NewHandler(runtimegateway.HandlerConfig{Authenticator: runtimegateway.NewAuthenticator(reader), Registry: registry})
			server := httptest.NewServer(http.HandlerFunc(handler.WS))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"?device_id=device&version="+proto.Version, http.Header{"Authorization": {"Bearer fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err = registry.WaitForDevice(t.Context(), "device", time.Second); err != nil {
				t.Fatal(err)
			}
			heartbeat, _ := proto.NewEnvelope(proto.TypeHeartbeat, "", proto.HeartbeatPayload{SupportedAgentKinds: []proto.SupportedAgentKind{{Kind: "codex", Available: true, Capabilities: prototest.Capabilities(proto.AgentKindCapabilities{Streaming: proto.CapabilitySupported, Steering: proto.CapabilitySupported, DurableTurns: proto.CapabilitySupported, DurableInputReceipts: proto.CapabilitySupported, Preparation: proto.CapabilitySupported, ExecutionControls: proto.CapabilitySupported, WebSearchControl: proto.CapabilitySupported, TextVerbosity: proto.CapabilitySupported, ToolObservations: proto.CapabilitySupported, SubagentControl: proto.CapabilitySupported, LocalEnvironment: proto.CapabilitySupported, WorkspaceReadPreparation: proto.CapabilitySupported, WorkspaceOutputExport: proto.CapabilitySupported})}}})
			if err = conn.WriteJSON(heartbeat); err != nil {
				t.Fatal(err)
			}
			select {
			case <-registry.CapabilityHints():
			case <-time.After(time.Second):
				t.Fatal("capability hint absent")
			}
			worker := &Worker{dispatcher: &Dispatcher{SessionsReader: reader, Registry: registry, DeploymentReader: &strictDeploymentReader{t: t, environmentAllocation: func(context.Context, deployment.AllocationKey) (deployment.Allocation, error) {
				return deployment.Allocation{}, deployment.ErrNotFound
			}}}, scheduleWake: make(chan struct{}, 1), concurrency: 1}
			storage := &initializationWakeStorage{reader: reader, worker: worker}
			if failed {
				storage.commitError = errors.New("fixture commit failure")
			}
			worker.dispatcher.sessionExecution, err = sessions.NewExecutionOperations(storage)
			if err != nil {
				t.Fatal(err)
			}
			schedule := workerSchedule{}
			if work, err := schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{}, true); err != nil || len(work) != 0 {
				t.Fatal("pending initialization selected work", err)
			}
			reader.scans = 0
			schedule.nextEnvironmentScan = time.Now().Add(time.Hour)
			if _, err = schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{}, false); err != nil {
				t.Fatal(err)
			}
			if reader.scans != 0 {
				t.Fatal("baseline poll bypassed scan deadline")
			}
			worker.initializeEnvironment(t.Context(), sessions.EnvironmentInitialization{EnvironmentID: "environment", SessionID: "session", DeviceID: "device"})
			if failed {
				if len(worker.scheduleWake) != 0 || reader.committed {
					t.Fatal("failed commit announced readiness")
				}
				return
			}
			if !reader.committed {
				t.Fatal("initialization did not commit")
			}
			select {
			case <-worker.scheduleWake:
			default:
				t.Fatal("committed initialization waits for fallback instead of waking scheduler")
			}
			active := map[string]bool{}
			work, err := schedule.selectWork(t.Context(), worker, []string{"device"}, active, true)
			if err != nil || len(work) != 1 || work[0].reservationID != "reservation" {
				t.Fatal("committed input not selected", work, err)
			}
			if duplicate, err := schedule.selectWork(t.Context(), worker, []string{"device"}, active, true); err != nil || len(duplicate) != 0 {
				t.Fatal("duplicate dispatch", duplicate, err)
			}
			if reader.scans != 2 {
				t.Fatal("ready hint did not bypass Environment scan throttle")
			}
			if _, err = schedule.selectWork(t.Context(), worker, []string{"device"}, map[string]bool{}, false); err != nil {
				t.Fatal(err)
			}
			if reader.scans != 2 {
				t.Fatal("ready hint disabled polling throttle")
			}
		})
	}
}
