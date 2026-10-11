package textobs

import "testing"

func TestAttemptLimitsExactSourceAndNeverCarriesReplayCandidate(t *testing.T) {
	ctx, first := Begin(t.Context(), "turn", 7)
	Capture(Source(ctx, "other", 7), "foreign")
	Capture(Source(ctx, "turn", 8), "later")
	Capture(ctx, "unscoped")
	if first.EventID() != "" {
		t.Fatal("unrelated event captured")
	}
	Capture(Source(ctx, "turn", 7), "candidate")
	Capture(Source(ctx, "turn", 7), "second")
	if first.EventID() != "candidate" {
		t.Fatal("first candidate lost")
	}
	_, replay := Begin(t.Context(), "turn", 7)
	if replay.EventID() != "" {
		t.Fatal("unknown prior attempt leaked into replay")
	}
	retryCtx, retry := Begin(t.Context(), "turn", 7)
	Capture(Source(retryCtx, "turn", 7), "reprojected")
	if retry.EventID() != "reprojected" || first.EventID() != "candidate" {
		t.Fatal("retry changed previous identity")
	}
}

func TestTerminalAttemptMatchesExactIdentityAndCannotCaptureText(t *testing.T) {
	ctx, a := BeginTerminal(t.Context(), "session", "turn", "completed")
	Capture(Source(ctx, "turn", 0), "text")
	for _, wrong := range [][3]string{{"other", "turn", "agent.session.turn.completed"}, {"session", "other", "agent.session.turn.completed"}, {"session", "turn", "agent.session.turn.failed"}, {"session", "turn", "agent.session.turn.output_text.done"}} {
		CaptureTerminal(ctx, wrong[0], wrong[1], wrong[2], "wrong")
	}
	if a.EventID() != "" {
		t.Fatal("unrelated event captured")
	}
	CaptureTerminal(ctx, "session", "turn", "agent.session.turn.completed", "actual")
	CaptureTerminal(ctx, "session", "turn", "agent.session.turn.completed", "later")
	if a.EventID() != "actual" {
		t.Fatal("terminal identity lost")
	}
	textCtx, text := Begin(t.Context(), "turn", 0)
	CaptureTerminal(textCtx, "session", "turn", "agent.session.turn.completed", "terminal")
	if text.EventID() != "" {
		t.Fatal("terminal contaminated first text")
	}
	_, retry := BeginTerminal(t.Context(), "session", "turn", "completed")
	if retry.EventID() != "" {
		t.Fatal("candidate copied to retry")
	}
}
