package api

import (
	"encoding/json"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"testing"
)

func TestProjectInputAvailabilitySeparatesTurnFailureFromEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, status, envType, envStatus, reason string
		pending, environmentFailure              bool
		activity, coreFailure                    string
	}{
		{name: "failed turn can continue", status: "failed", envType: "openai_hosted", envStatus: "connected"},
		{name: "no environment", status: "failed", envType: "none"},
		{name: "failed environment", status: "failed", envType: "openai_hosted", envStatus: "failed", reason: "environment_unavailable"},
		{name: "expired environment", status: "failed", envType: "self_hosted", envStatus: "expired", reason: "environment_unavailable"},
		{name: "disconnected", status: "failed", envType: "self_hosted", envStatus: "disconnected", reason: "environment_unavailable"},
		{name: "running", status: "in_progress", envType: "none", reason: "busy"},
		{name: "pending reservation", status: "idle", envType: "none", pending: true, reason: "busy"},
		{name: "requires action", status: "requires_action", envType: "none", reason: "requires_action"},
		{name: "missing provider", status: "failed", envType: "none", activity: "model_provider_required", reason: "configuration_required"},
		{name: "turn missing provider", status: "failed", envType: "none", coreFailure: "model_provider_required", reason: "configuration_required"},
		{name: "preparation recoverable", status: "failed", envType: "self_hosted", envStatus: "connected", activity: "runtime_preparation_failed"},
		{name: "environment failure wins", status: "failed", envType: "none", environmentFailure: true, reason: "environment_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := sessions.Session{PendingInput: tc.pending, LastTurn: &sessions.Turn{Status: sessions.TurnFailed}}
			if tc.envType != "none" {
				session.Environment = &sessions.Environment{Status: tc.envStatus}
			}
			if tc.environmentFailure {
				session.EnvironmentFailure = &sessions.EnvironmentFailure{}
			}
			if tc.activity != "" {
				session.EnvironmentInputActivity = &sessions.EnvironmentInputActivity{Status: "failed", Failure: tc.activity}
			}
			if tc.coreFailure != "" {
				session.LastTurn.Outcome = json.RawMessage(`{"error_code":"` + tc.coreFailure + `"}`)
			}
			available, reason := projectInputAvailability(session, tc.status, tc.envType)
			if available != (tc.reason == "") || reason != tc.reason {
				t.Fatal(available, reason)
			}
		})
	}
}

func TestProjectInputAvailabilityRejectsActiveTurnEvenWithStalePublicStatus(t *testing.T) {
	for _, status := range []string{sessions.TurnQueued, sessions.TurnInProgress, sessions.TurnWaiting} {
		available, reason := projectInputAvailability(sessions.Session{LastTurn: &sessions.Turn{Status: status}}, "failed", "none")
		if available || reason != "busy" {
			t.Fatal(status, available, reason)
		}
	}
	available, _ := projectInputAvailability(sessions.Session{}, "unknown", "none")
	if available {
		t.Fatal("unknown state is not permission")
	}
}
