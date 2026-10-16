package sessions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/items"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

type observedProjectionTx struct{ *fakeTx }

func (f observedProjectionTx) AppendChanges(ctx context.Context, changes ...SessionChange) error {
	for _, change := range changes {
		if change.Event.Type == "agent.session.turn.output_text.delta" {
			textobs.Capture(ctx, "generated-event")
		}
	}
	return nil
}
func TestProjectionScopesObservationToExactJournalSource(t *testing.T) {
	for _, sequence := range []int64{6, 7, 8} {
		index := int32(0)
		tx := observedProjectionTx{&fakeTx{t: t, loadItem: returns(items.Stored{}), putItem: func(items.Change) (*int32, error) { return &index, nil }}}
		ctx, a := textobs.Begin(t.Context(), testTurn, 7)
		err := ProjectSource(ctx, tx, Source{Turn: testTurn, Kind: "output_message", Sequence: sequence, Payload: json.RawMessage(`{"id":"m1","status":"completed","text":"hi"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if (a.EventID() != "") != (sequence == 7) {
			t.Fatalf("source %d: %q", sequence, a.EventID())
		}
	}
}
