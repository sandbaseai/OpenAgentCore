package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

type artifactWorkspaceConn struct {
	*executionTraceConn
	replies chan []byte
	failed  bool
}

func (c *artifactWorkspaceConn) ReadMessage() (int, []byte, error) {
	select {
	case raw := <-c.replies:
		return 1, raw, nil
	case <-c.closed:
		return 0, nil, io.EOF
	}
}
func (c *artifactWorkspaceConn) WriteMessage(_ int, raw []byte) error {
	var request proto.Envelope
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	status := proto.PreparationStatusPayload{Handle: "handle", State: "ready", Revision: 1}
	switch request.Type {
	case proto.TypeExecutionPrepare:
		if c.failed {
			status.State = "failed"
		}
	case proto.TypeExecutionRelease:
		status.State, status.Revision = "released", 2
	default:
		return errors.New("unexpected workspace request")
	}
	response, err := proto.NewEnvelope(proto.TypePreparationStatus, request.ID, status)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	c.replies <- encoded
	return nil
}

func TestArtifactWorkspaceScopePreservesPreparationAndRelease(t *testing.T) {
	for _, mode := range []string{"success", "consume_failure", "prepare_failure", "unscoped"} {
		t.Run(mode, func(t *testing.T) {
			logs := captureExecutionLogs(t)
			conn := &artifactWorkspaceConn{executionTraceConn: &executionTraceConn{closed: make(chan struct{})}, replies: make(chan []byte, 4), failed: mode == "prepare_failure"}
			peer := runtimegateway.NewSessionWithOwner(conn, "device", "tenant", "test", runtimegateway.NewRegistry(), func(string, ...any) {}, nil)
			peer.Start()
			defer peer.Close("test")
			session := sessions.Session{ID: "session", TenantID: "tenant", Engine: "codex"}
			environment := sessions.Environment{ID: "environment", SessionID: "session", TenantID: "tenant", Configuration: json.RawMessage(`{"type":"openai_hosted"}`)}
			ctx := t.Context()
			if mode != "unscoped" {
				ctx = withArtifactObservation(ctx, "session", "turn")
			}
			called := false
			consumeErr := errors.New("private consume error")
			err := (&Dispatcher{}).withPreparedWorkspace(ctx, peer, session, environment, sessions.ExecutionDevice{EnvironmentID: "environment"}, func(_ context.Context, handle string) error {
				called = true
				if handle != "handle" {
					t.Fatal(handle)
				}
				if mode == "consume_failure" {
					return consumeErr
				}
				return nil
			})
			if mode == "prepare_failure" {
				if !errors.Is(err, ErrExecutionUnavailable) || called {
					t.Fatal(err, called)
				}
			} else if mode == "consume_failure" {
				if !errors.Is(err, consumeErr) {
					t.Fatal(err)
				}
			} else if err != nil || !called {
				t.Fatal(err, called)
			}
			if mode == "unscoped" {
				if bytes.Contains(logs.Bytes(), []byte("artifact_workspace")) {
					t.Fatal("scope leaked to directory reads")
				}
				return
			}
			if bytes.Count(logs.Bytes(), []byte(`"stage":"artifact_workspace_prepare"`)) != 1 {
				t.Fatal("prepare count")
			}
			expected := "artifact_workspace_release"
			if mode == "prepare_failure" {
				expected = "artifact_workspace_cleanup"
			}
			if bytes.Count(logs.Bytes(), []byte(`"stage":"`+expected+`"`)) != 1 {
				t.Fatal("release/cleanup missing")
			}
			if bytes.Contains(logs.Bytes(), []byte("private consume error")) {
				t.Fatal("raw error leaked")
			}
		})
	}
}
