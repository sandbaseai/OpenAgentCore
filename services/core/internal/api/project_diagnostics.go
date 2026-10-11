package api

import (
	"context"
	"net/http"
	"time"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/go-chi/chi/v5"
)

// @Summary Retrieve Project Session diagnostics
// @Description OpenAgentCore extension. Project key only. Safe committed failure classification without private execution details.
// @Tags Diagnostics
// @Produce json
// @Security BearerAuth
// @Param session_id path string true "Session ID"
// @Success 200 {object} v1.SessionDiagnostics
// @Failure 400,401,404,500,503 {object} v1.ErrorResponse
// @Router /agents/sessions/{session_id}/diagnostics [get]
func (h *Handler) getProjectSessionDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	session, err := h.SessionAdmin.GetSessionDiagnosticsSnapshot(ctx, tenantID(r), chi.URLParam(r, "session_id"))
	if err != nil {
		writeSessionsError(w, r, err)
		return
	}
	public, err := sessionResponse(session, h.executorURL())
	if err != nil {
		writeSessionsError(w, r, err)
		return
	}
	snapshot := sessionDiagnosticResponse(session, public.ID, public.Status)
	response := v1.SessionDiagnostics{Object: "agent.session_diagnostics", SessionID: public.ID, Status: public.Status}
	available, reason := projectInputAvailability(session, public.Status, public.Environment.Type)
	response.InputAvailable, response.InputUnavailableReason = &available, reason
	if snapshot.Failure != nil {
		response.TurnID = snapshot.Failure.TurnID
		response.Diagnostic = projectDiagnostic(&snapshot.Failure.DiagnosticFailure, snapshot.Failure.Source)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

// A failed Turn is history, not a closed Session. This is a snapshot hint for
// starting a new Turn, never authorization to replay an input or bypass the
// transactional submission gate (including concurrent file writes).
func projectInputAvailability(session sessions.Session, status, environmentType string) (bool, string) {
	if session.EnvironmentFailure != nil {
		return false, "environment_unavailable"
	}
	if status == "requires_action" {
		return false, "requires_action"
	}
	if turn := session.LastTurn; turn != nil && (turn.Status == sessions.TurnQueued || turn.Status == sessions.TurnInProgress || turn.Status == sessions.TurnWaiting) {
		return false, "busy"
	}
	if session.PendingInput || status == "in_progress" {
		return false, "busy"
	}
	if environmentType != "none" {
		if session.Environment == nil || (session.Environment.Status != "connected" && session.Environment.Status != "ready") {
			return false, "environment_unavailable"
		}
	}
	if activity := session.EnvironmentInputActivity; activity != nil && activity.Failure == "model_provider_required" {
		return false, "configuration_required"
	}
	if turn := session.LastTurn; turn != nil && turn.Status == sessions.TurnFailed {
		if code, _ := sessions.DiagnosticFailureCode(turn.Outcome); code == "model_provider_required" {
			return false, "configuration_required"
		}
	}
	if status != "idle" && status != "failed" {
		return false, "configuration_required"
	}
	return true, ""
}

// @Summary Retrieve Project Turn diagnostics
// @Description OpenAgentCore extension. Project key only. Safe committed failure classification without private execution details.
// @Tags Diagnostics
// @Produce json
// @Security BearerAuth
// @Param session_id path string true "Session ID"
// @Param turn_id path string true "Root Turn ID"
// @Success 200 {object} v1.TurnDiagnostics
// @Failure 400,401,404,500,503 {object} v1.ErrorResponse
// @Router /agents/sessions/{session_id}/turns/{turn_id}/diagnostics [get]
func (h *Handler) getProjectTurnDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snapshot, err := h.SessionAdmin.GetTurnDiagnosticsSnapshot(ctx, tenantID(r), chi.URLParam(r, "session_id"), chi.URLParam(r, "turn_id"))
	if err != nil {
		writeSessionsError(w, r, err)
		return
	}
	public, err := turnResponse(snapshot.Session, snapshot.Turn)
	if err != nil {
		writeSessionsError(w, r, err)
		return
	}
	response := v1.TurnDiagnostics{Object: "agent.turn_diagnostics", SessionID: public.SessionID, TurnID: public.ID, Status: public.Status, Diagnostic: projectDiagnostic(turnDiagnosticFailure(snapshot.Turn), "turn")}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func projectDiagnostic(failure *DiagnosticFailure, source string) *v1.ExecutionDiagnostic {
	if failure == nil {
		return nil
	}
	code := failure.Code
	if code == "internal_error" {
		code = "unknown"
	}
	return &v1.ExecutionDiagnostic{Code: code, Source: source, FailedAt: failure.FailedAt}
}
