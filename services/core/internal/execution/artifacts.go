package execution

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimegateway"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

func (d *Dispatcher) captureCompletedArtifacts(ctx context.Context, peer *runtimegateway.Session, session sessions.Session, environment sessions.Environment, bound sessions.ExecutionDevice, turnID string, result Result, status string) (Result, string) {
	if status != sessions.TurnCompleted {
		observeArtifactSkip(ctx, session.ID, turnID, "turn_not_completed")
		return result, status
	}
	if !LocalWorkspaceConfiguration(environment.Configuration) {
		observeArtifactSkip(ctx, session.ID, turnID, "workspace_not_local")
		return result, status
	}
	owner, cancel := context.WithTimeout(ctx, proto.WorkspaceExportTimeout)
	defer cancel()
	owner = withArtifactObservation(owner, session.ID, turnID)
	captureStarted := time.Now()
	beginStarted := time.Now()
	err := d.sessionExecution.BeginTurnArtifactCapture(owner, session.TenantID, session.ID, turnID, result.AppliedThrough)
	observeArtifactWorkspace(owner, "artifact_capture_begin", beginStarted, err)
	defer func() { observeArtifactWorkspace(owner, "artifact_capture_total", captureStarted, err) }()
	if err == nil {
		err = d.withPreparedWorkspace(owner, peer, session, environment, bound, func(ctx context.Context, handle string) error {
			exportStarted := time.Now()
			exportErr := peer.ExportWorkspaceOutputs(ctx, proto.WorkspaceExportPayload{Handle: handle, EnvironmentID: environment.ID}, func(body io.Reader) error {
				started := time.Now()
				stageErr := d.Sessions.StageTurnArtifacts(ctx, sessions.StageTurnArtifactsCommand{TenantID: session.TenantID, SessionID: session.ID, TurnID: turnID, EnvironmentID: environment.ID, Export: body})
				observeArtifactWorkspace(ctx, "artifact_export_staging", started, stageErr)
				return stageErr
			})
			observeArtifactWorkspace(ctx, "artifact_export_total", exportStarted, exportErr)
			return exportErr
		})
	}
	if err == nil {
		return result, status
	}
	// Do not expose native diagnostics or publish partial output after a failed capture.
	result.ErrorCode = "artifact_capture_failed"
	if errors.Is(err, sessions.ErrUnappliedInputs) {
		result.ErrorCode = "input_not_applied"
	}
	status = sessions.TurnFailed
	check, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	checkStarted := time.Now()
	turn, checkErr := d.SessionsReader.GetTurn(check, session.TenantID, session.ID, turnID)
	observeArtifactWorkspace(owner, "artifact_failure_cancel_check", checkStarted, checkErr)
	if checkErr == nil && !turn.CancelRequestedAt.IsZero() {
		status = sessions.TurnCancelled
	}
	return result, status
}
