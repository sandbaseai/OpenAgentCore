package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/textobs"
)

type textObservationWriter struct {
	call   int
	replay bool
}

func (w *textObservationWriter) AppendTurnEvents(ctx context.Context, _, _, turn string, first int32, _ []sessions.ExecutionEvent) error {
	w.call++
	if w.call == 1 {
		textobs.Capture(textobs.Source(ctx, turn, int64(first)+1), "candidate-unknown")
		return context.DeadlineExceeded
	}
	if !w.replay {
		textobs.Capture(textobs.Source(ctx, turn, int64(first)+1), "candidate-reprojected")
	}
	return nil
}

func TestFirstTextPersistenceFailureRetryIdentityAndBoundedLogs(t *testing.T) {
	for _, replay := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown_commit_replay", false: "rollback_reproject"}[replay], func(t *testing.T) {
			out := captureExecutionLogs(t)
			writer := &textObservationWriter{replay: replay}
			j := journal{ctx: t.Context(), writer: writer, next: 1, session: "session", turn: "turn"}
			blank, _ := proto.NewEnvelope(proto.TypeDelta, "turn", proto.DeltaPayload{})
			text, _ := proto.NewEnvelope(proto.TypeDelta, "turn", proto.DeltaPayload{Delta: "SECRET-TEXT"})
			if err := j.enqueue(blank); err != nil {
				t.Fatal(err)
			}
			for range 10 {
				if err := j.enqueue(text); err != nil {
					t.Fatal(err)
				}
			}
			if err := j.flush(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if j.firstText.confirmed {
				t.Fatal("failed append confirmed")
			}
			if err := j.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !j.firstText.confirmed {
				t.Fatal("successful append missing")
			}
			if err := j.enqueue(text); err != nil {
				t.Fatal(err)
			}
			if err := j.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(out.Bytes(), []byte("SECRET-TEXT")) {
				t.Fatal("logged body")
			}
			var observations []map[string]any
			for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
				var row map[string]any
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				if row["msg"] == "journal first text persistence" {
					observations = append(observations, row)
				}
			}
			if len(observations) != 2 {
				t.Fatalf("observations=%v", observations)
			}
			failed, success := observations[0], observations[1]
			if failed["status"] != "unconfirmed" || failed["receipt_to_confirmation_ms"] != nil || failed["event_id"] != "candidate-unknown" {
				t.Fatal(failed)
			}
			if success["status"] != "confirmed" || success["journal_sequence"] != float64(2) || success["attempts"] != float64(2) {
				t.Fatal(success)
			}
			expected, status := "candidate-reprojected", "confirmed"
			if replay {
				expected, status = "", "unobserved"
			}
			if success["event_id"] != expected || success["mapping_status"] != status {
				t.Fatal(success)
			}
		})
	}
}

func TestFirstTextDrainAndEmptyOutput(t *testing.T) {
	captureExecutionLogs(t)
	j := journal{ctx: t.Context(), writer: &recoveringWriter{}, next: 1}
	frames := make(chan proto.Envelope, 2)
	empty, _ := proto.NewEnvelope(proto.TypeDelta, "turn", proto.DeltaPayload{})
	frames <- empty
	if err := j.drain(frames, &Result{}); err != nil {
		t.Fatal(err)
	}
	if !j.firstText.received.IsZero() {
		t.Fatal("empty text observed")
	}
	text, _ := proto.NewEnvelope(proto.TypeDelta, "turn", proto.DeltaPayload{Delta: "hello"})
	frames <- text
	close(frames)
	if err := j.drain(frames, &Result{}); err != nil {
		t.Fatal(err)
	}
	if j.firstText.sequence != 2 {
		t.Fatal("drained text missing")
	}
	if err := j.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !j.firstText.confirmed {
		t.Fatal("drained text not confirmed")
	}
}

type blockedTextWriter struct{ entered, release chan struct{} }

func (w blockedTextWriter) AppendTurnEvents(context.Context, string, string, string, int32, []sessions.ExecutionEvent) error {
	close(w.entered)
	<-w.release
	return nil
}
func TestFirstTextCannotConfirmBeforeAppendReturns(t *testing.T) {
	out := captureExecutionLogs(t)
	w := blockedTextWriter{make(chan struct{}), make(chan struct{})}
	j := journal{ctx: t.Context(), writer: w, next: 1}
	text, _ := proto.NewEnvelope(proto.TypeDelta, "turn", proto.DeltaPayload{Delta: "answer"})
	if err := j.enqueue(text); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- j.flush(t.Context()) }()
	<-w.entered
	if j.firstText.confirmed {
		t.Fatal("confirmed while append blocked")
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !j.firstText.confirmed || !bytes.Contains(out.Bytes(), []byte(`"status":"confirmed"`)) {
		t.Fatal("missing completion")
	}
}
