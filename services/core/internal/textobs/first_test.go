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
