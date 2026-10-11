package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgtest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
)

// Faults are injected around the real leased PostgreSQL transaction, after
// projection/insert. No test manufactures a public EventID or calls Capture.
type observedTransactionStorage struct {
	sessions.ExecutionStorage
	mode             string
	first            bool
	entered, release chan struct{}
	cancel           context.CancelFunc
}

func (s *observedTransactionStorage) WithTurns(ctx context.Context, tenant, session string, apply func(context.Context, sessions.TurnTx) error) error {
	first := !s.first
	s.first = true
	err := s.ExecutionStorage.WithTurns(ctx, tenant, session, func(ctx context.Context, tx sessions.TurnTx) error {
		if err := apply(ctx, tx); err != nil {
			return err
		}
		if first {
			switch s.mode {
			case "rollback":
				return errors.New("injected rollback after projection")
			case "cancel":
				s.cancel()
				return ctx.Err()
			case "delayed":
				close(s.entered)
				<-s.release
			}
		}
		return nil
	})
	if err == nil && first && s.mode == "unknown" {
		return context.DeadlineExceeded
	}
	return err
}

func TestFirstTextRealPostgresCommitUnknownRollbackReplayAndCancellation(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "rollback", "cancel", "delayed", "empty"} {
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
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			storage := &observedTransactionStorage{ExecutionStorage: sessionpg.NewExecution(lease), mode: mode, cancel: cancel, entered: make(chan struct{}), release: make(chan struct{})}
			operations, err := sessions.NewExecutionOperations(storage)
			if err != nil {
				t.Fatal(err)
			}
			reader := sessionpg.New(pgunit.NewPool(pool), nil)
			read := func() []sessions.SessionChange {
				t.Helper()
				changes, err := reader.ListSessionEvents(t.Context(), tenant, session, 0)
				if err != nil {
					t.Fatal(err)
				}
				return changes
			}
			j := journal{ctx: ctx, writer: operations, next: 1, tenant: tenant, session: session, turn: turn}
			// Public lifecycle events from an empty native message precede the first
			// text, so journal ordinal and Session event sequence differ in this batch.
			frames := []string{`{"id":"empty","status":"in_progress","text":""}`, `{"id":"answer","status":"completed","text":"PRIVATE-TEXT"}`, `{"id":"later","status":"completed","text":"later"}`}
			if mode == "empty" {
				frames = frames[:1]
			}
			for _, raw := range frames {
				if err := j.enqueue(proto.Envelope{Type: proto.TypeOutputMessage, Payload: json.RawMessage(raw)}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "delayed" {
				done := make(chan error, 1)
				go func() { done <- j.flush(ctx) }()
				<-storage.entered
				if len(read()) != 0 || j.firstText.confirmed {
					t.Fatal("uncommitted projection visible/confirmed")
				}
				close(storage.release)
				err = <-done
			} else {
				err = j.flush(ctx)
			}
			failed := mode == "unknown" || mode == "rollback" || mode == "cancel"
			if (err != nil) != failed {
				t.Fatalf("append error %v", err)
			}
			rows := persistenceRows(t, logs.Bytes())
			if mode == "empty" {
				if len(rows) != 0 || !j.firstText.received.IsZero() {
					t.Fatal("empty output claimed first text")
				}
				return
			}
			if len(rows) != 1 {
				t.Fatal(rows)
			}
			candidate, _ := rows[0]["event_id"].(string)
			if candidate == "" {
				t.Fatal("real projection did not capture candidate")
			}
			if failed && (rows[0]["status"] != "unconfirmed" || rows[0]["receipt_to_confirmation_ms"] != nil) {
				t.Fatal(rows[0])
			}
			before := read()
			if mode == "rollback" || mode == "cancel" {
				if len(before) != 0 {
					t.Fatal("rolled-back candidate became public")
				}
			} else if !containsObservedID(before, candidate) {
				t.Fatal("candidate not in authoritative SSE reader")
			}
			if mode == "cancel" {
				return
			}
			if mode == "unknown" || mode == "rollback" {
				if err := j.flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				rows = persistenceRows(t, logs.Bytes())
				if len(rows) != 2 || rows[1]["status"] != "confirmed" {
					t.Fatal(rows)
				}
				if mode == "unknown" {
					if rows[1]["mapping_status"] != "unobserved" || rows[1]["event_id"] != "" {
						t.Fatal("replay promoted old candidate", rows[1])
					}
					if len(read()) != len(before) || !containsObservedID(read(), candidate) {
						t.Fatal("replay duplicated/replaced committed events")
					}
				} else {
					replacement, _ := rows[1]["event_id"].(string)
					if replacement == "" || replacement == candidate || rows[1]["mapping_status"] != "confirmed" || containsObservedID(read(), candidate) || !containsObservedID(read(), replacement) {
						t.Fatal("rollback retry mapping is not exact", rows)
					}
				}
			} else {
				if rows[0]["mapping_status"] != "confirmed" {
					t.Fatal(rows)
				}
				for _, change := range read() {
					if change.Event.EventID == candidate && change.Sequence == int64(j.firstText.sequence) {
						t.Fatal("fixture did not distinguish sequence domains")
					}
				}
			}
			beforeDuplicate := len(read())
			if err := j.enqueue(proto.Envelope{Type: proto.TypeOutputMessage, Payload: json.RawMessage(`{"id":"answer","status":"completed","text":"PRIVATE-TEXT"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := j.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(read()) != beforeDuplicate || len(persistenceRows(t, logs.Bytes())) != len(rows) {
				t.Fatal("unchanged projection repeated first-text observation")
			}
			if bytes.Contains(logs.Bytes(), []byte("PRIVATE-TEXT")) {
				t.Fatal("logged body")
			}
		})
	}
}

func persistenceRows(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var rows []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "journal first text persistence" {
			rows = append(rows, row)
		}
	}
	return rows
}
func containsObservedID(changes []sessions.SessionChange, id string) bool {
	for _, change := range changes {
		if change.Event.EventID == id && change.Sequence > 0 {
			return true
		}
	}
	return false
}
