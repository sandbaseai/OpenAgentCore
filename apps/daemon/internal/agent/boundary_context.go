package agent

import (
	"context"
	"time"
)

type boundaryInputKey struct{}
type BoundaryInput struct {
	Received      time.Time
	PreparationID string
}

// WithBoundaryInput carries local observation only; it adds no protocol fields.
func WithBoundaryInput(ctx context.Context, received time.Time, preparation string) context.Context {
	return context.WithValue(ctx, boundaryInputKey{}, BoundaryInput{received, preparation})
}
func BoundaryInputFrom(ctx context.Context) BoundaryInput {
	value, _ := ctx.Value(boundaryInputKey{}).(BoundaryInput)
	return value
}
