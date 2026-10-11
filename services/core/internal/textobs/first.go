// Package textobs carries bounded, private text and terminal diagnostics
// through one write attempt. It never changes stored or public events.
package textobs

import "context"

type attemptKey struct{}
type sourceKey struct{}

// Attempt belongs to one AppendTurnEvents or CompleteExecution call. A retry
// gets a new Attempt; success never confirms an earlier candidate event ID.
type Attempt struct {
	turn                  string
	sequence              int64
	eventID               string
	session, terminalType string
}

func Begin(ctx context.Context, turn string, sequence int64) (context.Context, *Attempt) {
	a := &Attempt{turn: turn, sequence: sequence}
	return context.WithValue(ctx, attemptKey{}, a), a
}

// Source limits capture to the exact execution journal entry being projected.
func Source(ctx context.Context, turn string, sequence int64) context.Context {
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, sourceKey{}, a.turn == turn && a.sequence == sequence)
}

// Capture is called only after the event insert returns successfully, still
// inside the transaction. This is a candidate, not evidence of a commit.
func Capture(ctx context.Context, eventID string) {
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	selected, _ := ctx.Value(sourceKey{}).(bool)
	if a != nil && a.terminalType == "" && selected && a.eventID == "" {
		a.eventID = eventID
	}
}

func (a *Attempt) EventID() string {
	if a == nil {
		return ""
	}
	return a.eventID
}

// BeginTerminal scopes one CompleteExecution attempt independently of the
// execution journal projection. Failure never promotes an inserted candidate.
func BeginTerminal(ctx context.Context, session, turn, status string) (context.Context, *Attempt) {
	a := &Attempt{session: session, turn: turn, terminalType: "agent.session.turn." + status}
	return context.WithValue(ctx, attemptKey{}, a), a
}

// CaptureTerminal runs after the insert but before transaction confirmation.
func CaptureTerminal(ctx context.Context, session, turn, kind, eventID string) {
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil || a.eventID != "" || a.session != session || a.turn != turn || a.terminalType != kind {
		return
	}
	switch kind {
	case "agent.session.turn.completed", "agent.session.turn.failed", "agent.session.turn.cancelled":
		a.eventID = eventID
	}
}
