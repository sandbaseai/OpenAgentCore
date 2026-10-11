package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgtest"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/sessionpg"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
	"github.com/google/uuid"
)

func TestStreamTextRealPostgresTwoTurnsSameConnection(t *testing.T) {
	pool := pgtest.OpenIsolated(t, nil)
	tenant, session := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,tenant_id,engine,idempotency_key,request_hash) VALUES($1,$2,'codex','key','hash')`, session, tenant); err != nil {
		t.Fatal(err)
	}
	lease, err := pgunit.AcquireLease(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close(context.Background()) })
	operations, err := sessions.NewExecutionOperations(sessionpg.NewExecution(lease))
	if err != nil {
		t.Fatal(err)
	}
	reader := sessionpg.New(pgunit.NewPool(pool), nil)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	o := newStreamTextObservation(t.Context(), session, 0)
	w := &observedStreamWriter{header: http.Header{}}
	write := openEventStream(w, 200, o)
	if write == nil {
		t.Fatal("open")
	}
	cursor := int64(0)
	observed := map[string]int64{}
	for range 2 {
		turn := uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO turns(id,session_id,status) VALUES($1,$2,'in_progress')`, turn, session); err != nil {
			t.Fatal(err)
		}
		ctx, attempt := textobs.Begin(t.Context(), turn, 2)
		batch := []sessions.ExecutionEvent{
			{Kind: proto.TypeOutputMessage, Payload: json.RawMessage(`{"id":"empty","status":"in_progress","text":""}`)},
			{Kind: proto.TypeOutputMessage, Payload: json.RawMessage(`{"id":"answer","status":"completed","text":"private-answer"}`)},
		}
		if err := operations.AppendTurnEvents(ctx, tenant, session, turn, 1, batch); err != nil {
			t.Fatal(err)
		}
		if attempt.EventID() == "" {
			t.Fatal("missing real inserted event")
		}
		changes, err := reader.ListSessionEvents(t.Context(), tenant, session, cursor)
		if err != nil {
			t.Fatal(err)
		}
		readAt := time.Now()
		matched := false
		for _, change := range changes {
			event, err := streamResponse(sessions.Session{ID: session, Configuration: json.RawMessage(`{"agent":{"id":"agent_fixture","model":"fixture","tools":[]},"environment":{"type":"none"}}`)}, change, "")
			if err != nil {
				t.Fatal(err)
			}
			o.begin(event, change.Sequence, readAt)
			if event.EventID == attempt.EventID() {
				if !o.active {
					t.Fatal("committed text not observed on same connection")
				}
				matched = true
				observed[event.EventID] = change.Sequence
			}
			err = emitSessionEvent(write, session, event)
			o.end(err)
			if err != nil {
				t.Fatal(err)
			}
			cursor = change.Sequence
		}
		if !matched {
			t.Fatal("collector ID not returned by SSE reader")
		}
		// Complete through the real settlement transaction; its terminal event
		// must match the collector and traverse this same live connection.
		terminalCtx, terminal := textobs.BeginTerminal(t.Context(), session, turn, sessions.TurnCompleted)
		if _, err := operations.CompleteExecution(terminalCtx, tenant, session, turn, sessions.TurnCompleted, nil, "", 0); err != nil {
			t.Fatal(err)
		}
		terminalChanges, err := reader.ListSessionEvents(t.Context(), tenant, session, cursor)
		if err != nil {
			t.Fatal(err)
		}
		matched = false
		for _, change := range terminalChanges {
			event, err := streamResponse(sessions.Session{ID: session, Configuration: json.RawMessage(`{"agent":{"id":"agent_fixture","model":"fixture","tools":[]},"environment":{"type":"none"}}`)}, change, "")
			if err != nil {
				t.Fatal(err)
			}
			o.begin(event, change.Sequence, time.Now())
			if event.EventID == terminal.EventID() {
				if !o.active || !o.terminal || event.Type != "agent.session.turn.completed" {
					t.Fatal("terminal identity not selected")
				}
				observed[event.EventID] = change.Sequence
				matched = true
			}
			err = emitSessionEvent(write, session, event)
			o.end(err)
			if err != nil {
				t.Fatal(err)
			}
			cursor = change.Sequence
		}
		if !matched {
			t.Fatal("real terminal missing from SSE")
		}
	}
	reads, outputs := 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		switch row["msg"] {
		case "session stream first text read", "session stream terminal read":
			reads++
		case "session stream first text output", "session stream terminal output":
			outputs++
		default:
			continue
		}
		id, _ := row["event_id"].(string)
		if observed[id] == 0 || row["session_event_sequence"] != float64(observed[id]) || row["connection_id"] != o.connection {
			t.Fatal("log lost authoritative identity", row)
		}
		if (row["msg"] == "session stream first text output" || row["msg"] == "session stream terminal output") && (row["write_status"] != "success" || row["flush_status"] != "success") {
			t.Fatal(row)
		}
		if !bytes.Contains(w.body.Bytes(), []byte(`"event_id":"`+id+`"`)) {
			t.Fatal("logged ID not on actual SSE wire")
		}
	}
	if reads != 4 || outputs != 4 {
		t.Fatalf("cold/warm read=%d output=%d", reads, outputs)
	}
	if bytes.Contains(logs.Bytes(), []byte("private-answer")) {
		t.Fatal("logged text")
	}
}
