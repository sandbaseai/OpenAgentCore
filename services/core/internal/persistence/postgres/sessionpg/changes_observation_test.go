package sessionpg

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/db/sqlc"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type observationInsertDB struct {
	sqlc.DBTX
	events []sessions.SessionChange
	fail   bool
}

func (d *observationInsertDB) Exec(_ context.Context, _ string, args ...interface{}) (pgconn.CommandTag, error) {
	if d.fail {
		return pgconn.CommandTag{}, errors.New("insert failed")
	}
	var change sessions.SessionChange
	if err := json.Unmarshal(args[1].([]byte), &change); err != nil {
		return pgconn.CommandTag{}, err
	}
	d.events = append(d.events, change)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestAppendChangesCapturesExactGeneratedCandidateOnlyAfterInsert(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "failure", false: "success"}[fail], func(t *testing.T) {
			ctx, a := textobs.Begin(t.Context(), "turn", 7)
			ctx = textobs.Source(ctx, "turn", 7)
			db := &observationInsertDB{fail: fail}
			text := "private-body"
			changes := []sessions.SessionChange{
				{Event: v1.SessionEvent{Type: "agent.session.turn.item.added"}},
				{Event: v1.SessionEvent{Type: "agent.session.turn.output_text.delta", Delta: &text}},
				{Event: v1.SessionEvent{Type: "agent.session.turn.output_text.done", Text: &text}},
			}
			err := AppendChanges(ctx, sqlc.New(db), pgtype.UUID{Bytes: uuid.New(), Valid: true}, changes...)
			if fail {
				if err == nil || a.EventID() != "" {
					t.Fatal("failed insert captured", err, a.EventID())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(db.events) != 3 || a.EventID() != db.events[1].Event.EventID || a.EventID() == db.events[2].Event.EventID {
				t.Fatal("candidate does not match inserted first text")
			}
			if changes[1].Event.EventID != "" {
				t.Fatal("changed caller's event")
			}
		})
	}
}
