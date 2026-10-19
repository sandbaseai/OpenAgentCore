package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/projects"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimedevice"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQAStreamBusyPrefixAndAuthorityFailure(t *testing.T) {
	for _, mode := range []string{"busy", "authority_failure", "list_failure"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
			t.Cleanup(func() { slog.SetDefault(previous) })
			key := callerBinding()
			key.TokenSHA256 = runtimedevice.HashCredential("fixture")
			f := &streamFixture{session: sessions.Session{ID: uuid.NewString(), TenantID: key.TenantID, CreatedAt: time.Now(), Metadata: map[string]string{}, Configuration: json.RawMessage(`{"agent":{"id":"agent_fixture","model":"fixture","tools":[]},"environment":{"type":"none"}}`)}}
			deps, fakes := testDependencies(t)
			f.serve(fakes)
			keys := projectKeys(t, key)
			checks := 0
			fakes.projectsReader.resolveAPIKey = func(ctx context.Context, digest [sha256.Size]byte) (projects.KeyBinding, error) {
				checks++
				if mode == "authority_failure" && checks > 1 {
					time.Sleep(3 * time.Millisecond)
					return projects.KeyBinding{}, errors.New("QA-SECRET")
				}
				return keys.ResolveAPIKey(ctx, digest)
			}
			calls := 0
			fakes.sessionEvents.listSessionEvents = func(context.Context, string, string, int64) ([]sessions.SessionChange, error) {
				calls++
				if mode != "busy" || calls > 2 {
					return nil, sessions.ErrNotFound
				}
				text := "QA-SECRET"
				return []sessions.SessionChange{{Sequence: int64(calls + 10), Event: v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: uuid.NewString(), EventID: uuid.NewString(), Delta: &text}}}, nil
			}
			request := httptest.NewRequest("GET", "/v1/agents/sessions/"+f.session.ID+"/events", nil)
			request.Header.Set("Authorization", "Bearer fixture")
			request.Header.Set("OpenAI-Beta", "agents=v1")
			newTestHandler(t, deps).ServeHTTP(&observedStreamWriter{header: http.Header{}}, request)
			count := 0
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				var row map[string]any
				if json.Unmarshal(line, &row) != nil {
					continue
				}
				if mode == "busy" && row["msg"] == "session stream first text read" {
					count++
					if row["list_events_count"] != float64(count) || row["empty_read_count"] != float64(0) || row["poll_wait_count"] != float64(0) || row["poll_wait_total_ms"] != nil || row["last_empty_to_first_read_ms"] != nil {
						t.Fatal(row)
					}
				}
				if mode != "busy" && row["msg"] == "session stream observation ended" {
					count++
					if row["outcome"] != "error" || row["text_observed"] != false || row["first_text_list_ms"] != nil {
						t.Fatal(row)
					}
				}
			}
			want := 1
			if mode == "busy" {
				want = 2
			}
			if count != want {
				t.Fatalf("logs=%d expected=%d", count, want)
			}
			if bytes.Contains(logs.Bytes(), []byte("QA-SECRET")) {
				t.Fatal("canary leaked")
			}
		})
	}
}
