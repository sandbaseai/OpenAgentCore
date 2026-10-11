// Package textobs carries a bounded, private first-text diagnostic through a
// single journal append attempt. It never changes stored or public events.
package textobs

import "context"

type attemptKey struct{}
type sourceKey struct{}

// Attempt belongs to one synchronous AppendTurnEvents call. A retry gets a new
// Attempt: replay confirms journal identity, not an earlier candidate event ID.
type Attempt struct {
	turn     string
	sequence int64
	eventID  string
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
	if a != nil && selected && a.eventID == "" {
		a.eventID = eventID
	}
}

func (a *Attempt) EventID() string {
	if a == nil {
		return ""
	}
	return a.eventID
}
