package api

import (
	"context"
	"encoding/json"
	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicInputReceipts(t *testing.T) {
	for _, state := range []string{"admitted", "pending", "failed", "absent", "missing"} {
		t.Run(state, func(t *testing.T) {
			var gotTenant string
			h, _, tenant := testHandler(t, func(_ *Dependencies, f *testFakes) {
				f.sessionsReader.getInputReceipts = func(_ context.Context, tenant, session, key string) (v1.SessionInputReceipts, error) {
					gotTenant = tenant
					if session != "session-id" || key != "Exact-Key" {
						t.Fatalf("incorrect identity %q %q", session, key)
					}
					if state == "missing" {
						return v1.SessionInputReceipts{}, sessions.ErrNotFound
					}
					return v1.SessionInputReceipts{Object: "agent.session.input_receipts", SessionID: session, Status: v1.InputReceiptStatus(state), Receipts: []v1.SessionInputReceipt{}}, nil
				}
			})
			r := httptest.NewRequest(http.MethodGet, "/v1/agents/sessions/session-id/input-receipts", nil)
			r.Header.Set("Authorization", "Bearer test-api-key")
			r.Header.Set("OpenAI-Beta", "agents=v1")
			r.Header.Set("Idempotency-Key", "Exact-Key")
			r.Header.Set("X-Tenant-ID", "forged")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 200
			if state == "pending" {
				want = 202
			}
			if state == "missing" {
				want = 404
			}
			if w.Code != want || gotTenant != tenant {
				t.Fatalf("status %d tenant %q: %s", w.Code, gotTenant, w.Body)
			}
			if state != "missing" {
				var got v1.SessionInputReceipts
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Receipts == nil || string(got.Status) != state || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("invalid receipt %s %v", w.Body, err)
				}
			}
		})
	}
}
