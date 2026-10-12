package textobs

import (
	"context"

	"github.com/google/uuid"
)

type artifactKey struct{}

// ArtifactScope identifies one capture, not a transaction or export receipt.
// Its immutable value is carried only by the completed-artifact path.
type ArtifactScope struct{ Session, Turn, Capture string }

func WithArtifact(ctx context.Context, session, turn string) context.Context {
	return context.WithValue(ctx, artifactKey{}, ArtifactScope{Session: session, Turn: turn, Capture: uuid.NewString()})
}

func Artifact(ctx context.Context) (ArtifactScope, bool) {
	scope, ok := ctx.Value(artifactKey{}).(ArtifactScope)
	return scope, ok
}
