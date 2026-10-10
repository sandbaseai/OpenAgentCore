package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/identity"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimedevice"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
)

func TestProjectDiagnosticsSafeProjection(t *testing.T) {
	id, turn := uuid.NewString(), uuid.NewString()
	session := sessions.Session{ID: id, Configuration: json.RawMessage(`{"agent":{"id":"agent_root","model":"test"},"environment":{"type":"none"}}`), LastTurn: &sessions.Turn{ID: turn, SessionID: id, Status: sessions.TurnFailed, Outcome: json.RawMessage(`{"error_code":"engine_failed","engine_error_code":"rate_limit_exceeded","error":"secret-canary","engine_http_status":429}`)}}
	deps, fakes := testDependencies(t)
	key := callerBinding()
	fakes.projectsReader.resolveAPIKey = projectKeys(t, key).ResolveAPIKey
	serveDiagnostics(diagnosticSnapshotStore{session: session})(&deps, fakes)
	h := newTestHandler(t, deps)
	for _, suffix := range []string{"/diagnostics", "/turns/" + turn + "/diagnostics"} {
		path := "/v1/agents/sessions/" + id + suffix
		for _, token := range []string{"", "Bearer admin"} {
			if w := diagnosticRequest(h, path, token); w.Code != 401 {
				t.Fatalf("credential isolation: %d", w.Code)
			}
		}
		w := diagnosticRequest(h, path, "Bearer caller")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"code":"rate_limit_exceeded"`) || !strings.Contains(body, `"source":"turn"`) || !strings.Contains(body, `"failed_at":null`) {
			t.Fatal(body)
		}
		for _, forbidden := range []string{"canary", "params", "items", "engine_http_status"} {
			if strings.Contains(body, forbidden) {
				t.Fatal(body)
			}
		}
	}
}

func TestProjectDiagnosticsReadErrorIsNotExecutionFailure(t *testing.T) {
	for _, err := range []error{sessions.ErrNotFound, errors.New("private-secret-canary")} {
		deps, fakes := testDependencies(t)
		key := callerBinding()
		fakes.projectsReader.resolveAPIKey = projectKeys(t, key).ResolveAPIKey
		fakes.sessionsReader.getSession = func(_ context.Context, tenant, id string) (sessions.Session, error) {
			if tenant != key.TenantID {
				t.Fatal("wrong tenant")
			}
			return sessions.Session{}, err
		}
		fakes.sessionAdmin.getTurnDiagnosticsSnapshot = func(_ context.Context, tenant, session, turn string) (sessions.TurnDiagnosticsSnapshot, error) {
			if tenant != key.TenantID {
				t.Fatal("wrong tenant")
			}
			return sessions.TurnDiagnosticsSnapshot{}, err
		}
		h := newTestHandler(t, deps)
		for _, suffix := range []string{"/diagnostics", "/turns/" + uuid.NewString() + "/diagnostics"} {
			w := diagnosticRequest(h, "/v1/agents/sessions/"+uuid.NewString()+suffix, "Bearer caller")
			want := 500
			if errors.Is(err, sessions.ErrNotFound) {
				want = 404
			}
			if w.Code != want || strings.Contains(w.Body.String(), "canary") || strings.Contains(w.Body.String(), `"diagnostic"`) {
				t.Fatal(w.Code, w.Body)
			}
		}
	}
}

func TestProjectDiagnosticUnknownAndNonFailure(t *testing.T) {
	if got := projectDiagnostic(nil, "turn"); got != nil {
		t.Fatal(got)
	}
	got := projectDiagnostic(turnDiagnosticFailure(sessions.Turn{Status: sessions.TurnFailed, Outcome: json.RawMessage(`{"error_code":"unrecognized-secret-canary"}`)}), "turn")
	if got.Code != "unknown" {
		t.Fatal(got)
	}
}

func TestProjectDiagnosticsDatabaseIsolation(t *testing.T) {
	service, pool := diagnosticDatabase(t)
	key, foreign := callerBinding(), callerBinding()
	foreign.TokenSHA256 = runtimedevice.HashCredential("foreign")
	deps, fakes := testDependencies(t)
	fakes.projectsReader.resolveAPIKey = projectKeys(t, key, foreign).ResolveAPIKey
	databaseSessionReads(pool)(&deps, fakes)
	handler := newTestHandler(t, deps)
	created, err := service.CreateSession(t.Context(), key.TenantID, sessions.CreateSession{Creator: identity.Subject{Kind: "service_account", ID: "test"}, Engine: "codex", IdempotencyKey: uuid.NewString(), Configuration: json.RawMessage(`{"agent":{"id":"agent_root","model":"test"},"environment":{"type":"none"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	session := created.Session.ID
	receipt := submitMessage(t, pool, key.TenantID, session, "message", json.RawMessage(`{"text":"private-canary"}`))
	paths := []string{"/v1/agents/sessions/" + session + "/diagnostics", "/v1/agents/sessions/" + session + "/turns/" + receipt.TurnID + "/diagnostics"}
	for _, path := range paths {
		if w := diagnosticRequest(handler, path, "Bearer caller"); w.Code != 200 || !strings.Contains(w.Body.String(), `"diagnostic":null`) {
			t.Fatal(w.Code, w.Body)
		}
		if w := diagnosticRequest(handler, path, "Bearer foreign"); w.Code != 404 {
			t.Fatal("foreign resource visible", w.Code, w.Body)
		}
	}
	other, err := service.CreateSession(t.Context(), key.TenantID, sessions.CreateSession{Creator: identity.Subject{Kind: "service_account", ID: "test"}, Engine: "codex", IdempotencyKey: uuid.NewString(), Configuration: json.RawMessage(`{"agent":{"id":"agent_root","model":"test"},"environment":{"type":"none"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	wrongParent := "/v1/agents/sessions/" + other.Session.ID + "/turns/" + receipt.TurnID + "/diagnostics"
	if w := diagnosticRequest(handler, wrongParent, "Bearer caller"); w.Code != 404 {
		t.Fatal("turn visible through another session", w.Code, w.Body)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE sessions SET deleted_at=clock_timestamp() WHERE id=$1", session); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if w := diagnosticRequest(handler, path, "Bearer caller"); w.Code != 404 {
			t.Fatal("deleted visible", w.Code, w.Body)
		}
	}
}

func TestProjectTurnDiagnosticsWaiting(t *testing.T) {
	id, turn := uuid.NewString(), uuid.NewString()
	session := sessions.Session{ID: id, Configuration: json.RawMessage(`{"agent":{"id":"agent_root","model":"test"},"environment":{"type":"none"}}`), LastTurn: &sessions.Turn{ID: turn, SessionID: id, Status: sessions.TurnWaiting}}
	deps, fakes := testDependencies(t)
	fakes.projectsReader.resolveAPIKey = projectKeys(t, callerBinding()).ResolveAPIKey
	serveDiagnostics(diagnosticSnapshotStore{session: session})(&deps, fakes)
	w := diagnosticRequest(newTestHandler(t, deps), "/v1/agents/sessions/"+id+"/turns/"+turn+"/diagnostics", "Bearer caller")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"waiting"`) || !strings.Contains(w.Body.String(), `"diagnostic":null`) {
		t.Fatal(w.Code, w.Body)
	}
}
