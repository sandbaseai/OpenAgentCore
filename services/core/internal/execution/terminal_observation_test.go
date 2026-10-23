package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/modelconfiguration"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgtest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
)

type terminalProviderObserver struct{}

func (terminalProviderObserver) ObserveDeploymentModelProvider(context.Context, modelconfiguration.Observation) (int64, error) {
	return 0, nil
}

func terminalRows(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["stage"] == "turn_completion" {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestTerminalRealCommitUnknownRollbackAndStatus(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "cancelled", "unknown", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			logs := captureExecutionLogs(t)
			pool := pgtest.OpenIsolated(t, nil)
			tenant, session, turn := uuid.NewString(), uuid.NewString(), uuid.NewString()
			if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,tenant_id,engine,idempotency_key,request_hash) VALUES($1,$2,'codex','key','hash')`, session, tenant); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO turns(id,session_id,status) VALUES($1,$2,'in_progress')`, turn, session); err != nil {
				t.Fatal(err)
			}
			lease, err := pgunit.AcquireLease(t.Context(), pool)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lease.Close(context.Background()) })
			storage := &observedTransactionStorage{ExecutionStorage: sessionpg.NewExecution(lease), mode: mode}
			operations, err := sessions.NewExecutionOperations(storage)
			if err != nil {
				t.Fatal(err)
			}
			d := Dispatcher{sessionExecution: operations, Observer: terminalProviderObserver{}}
			reader := sessionpg.New(pgunit.NewPool(pool), nil)
			read := func() []sessions.SessionChange {
				changes, err := reader.ListSessionEvents(t.Context(), tenant, session, 0)
				if err != nil {
					t.Fatal(err)
				}
				return changes
			}
			status := mode
			if mode == "unknown" || mode == "rollback" {
				status = sessions.TurnCompleted
			}
			ended, err := d.finishRun(tenant, session, turn, Result{}, status)
			uncertain := mode == "unknown" || mode == "rollback"
			if (err != nil) != uncertain {
				t.Fatal(err)
			}
			rows := terminalRows(t, logs.Bytes())
			if len(rows) != 1 {
				t.Fatal(rows)
			}
			candidate, _ := rows[0]["event_id"].(string)
			if candidate == "" {
				t.Fatal("no actual terminal candidate")
			}
			if uncertain && rows[0]["confirmation"] != "unconfirmed" {
				t.Fatal(rows)
			}
			if mode == "rollback" {
				if len(read()) != 0 {
					t.Fatal("rollback exposed candidate")
				}
			} else {
				found := false
				for _, change := range read() {
					if change.Event.EventID == candidate {
						found = change.Event.Type == "agent.session.turn."+status && change.Event.TurnID == turn && change.Sequence > 0
					}
				}
				if !found {
					t.Fatal("actual terminal ID/type/status not in authoritative SSE reader")
				}
			}
			if !uncertain && (ended.Status != status || rows[0]["confirmation"] != "confirmed") {
				t.Fatal(ended.Status, rows)
			}
			if uncertain {
				_, retryErr := d.finishRun(tenant, session, turn, Result{}, status)
				rows = terminalRows(t, logs.Bytes())
				if len(rows) != 2 {
					t.Fatal(rows)
				}
				if mode == "unknown" {
					if !errors.Is(retryErr, sessions.ErrTurnConflict) || rows[1]["confirmation"] != "unconfirmed" || rows[1]["event_id"] != "" {
						t.Fatal("terminal replay invented confirmation", retryErr, rows)
					}
				} else {
					replacement, _ := rows[1]["event_id"].(string)
					if retryErr != nil || replacement == candidate || !containsObservedID(read(), replacement) || containsObservedID(read(), candidate) || rows[1]["confirmation"] != "confirmed" {
						t.Fatal(retryErr, rows)
					}
				}
			}
		})
	}
}

func TestArtifactObservationSkipsWithoutSideEffects(t *testing.T) {
	logs := captureExecutionLogs(t)
	d := Dispatcher{}
	session := sessions.Session{ID: "session"}
	for _, test := range []struct{ status, configuration string }{{sessions.TurnFailed, `{"type":"openai_hosted"}`}, {sessions.TurnCompleted, `{"type":"none"}`}} {
		result, status := d.captureCompletedArtifacts(t.Context(), nil, session, sessions.Environment{Configuration: json.RawMessage(test.configuration)}, sessions.ExecutionDevice{}, "turn", Result{}, test.status)
		if status != test.status || result.ErrorCode != "" {
			t.Fatal("skip changed result")
		}
	}
	if bytes.Count(logs.Bytes(), []byte(`"msg":"artifact capture skipped"`)) != 2 {
		t.Fatal("skip not observed")
	}
}

func TestTerminalUnappliedFallbackUsesSeparateAttempts(t *testing.T) {
	logs := captureExecutionLogs(t)
	f := newFinishObservationFixture(t, 0)
	receipt := f.start(t)
	ended, err := f.dispatcher.finishRun(f.tenant, f.session.ID, receipt.TurnID, Result{}, sessions.TurnCompleted)
	if err != nil || ended.Status != sessions.TurnFailed {
		t.Fatal(err, ended.Status)
	}
	rows := terminalRows(t, logs.Bytes())
	if len(rows) != 2 || rows[0]["attempt"] != float64(1) || rows[0]["event_id"] != "" || rows[0]["confirmation"] != "unconfirmed" || rows[1]["attempt"] != float64(2) || rows[1]["confirmation"] != "confirmed" || rows[1]["requested_turn_status"] != sessions.TurnFailed {
		t.Fatal(rows)
	}
}

func TestArtifactCaptureFailureChecksCancellationWithoutExport(t *testing.T) {
	logs := captureExecutionLogs(t)
	f := newFinishObservationFixture(t, 0)
	receipt := f.start(t)
	if _, err := f.pool.Exec(t.Context(), `UPDATE turns SET cancel_requested_at=clock_timestamp() WHERE id=$1`, receipt.TurnID); err != nil {
		t.Fatal(err)
	}
	f.dispatcher.SessionsReader = sessionpg.New(pgunit.NewPool(f.pool), nil)
	result, status := f.dispatcher.captureCompletedArtifacts(t.Context(), nil, f.session, sessions.Environment{Configuration: json.RawMessage(`{"type":"openai_hosted"}`)}, sessions.ExecutionDevice{}, receipt.TurnID, Result{AppliedThrough: receipt.Sequence}, sessions.TurnCompleted)
	if status != sessions.TurnCancelled || result.ErrorCode != "artifact_capture_failed" {
		t.Fatal(status, result.ErrorCode)
	}
	for _, stage := range []string{"artifact_capture_begin", "artifact_failure_cancel_check", "artifact_capture_total"} {
		if !bytes.Contains(logs.Bytes(), []byte(`"stage":"`+stage+`"`)) {
			t.Fatal("missing stage", stage)
		}
	}
	if bytes.Contains(logs.Bytes(), []byte(`"stage":"artifact_workspace_prepare"`)) {
		t.Fatal("failed admission exported")
	}
}
