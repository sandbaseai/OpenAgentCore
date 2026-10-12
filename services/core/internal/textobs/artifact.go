package textobs

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"
)

type artifactKey struct{}

// ArtifactScope identifies one capture, not a transaction or export receipt.
// Its immutable value is carried only by the completed-artifact path.
type ArtifactScope struct {
	Session, Turn, Capture string
	transport              *artifactTransportClaim
}
type artifactTransportClaim struct{ claimed, unobserved atomic.Bool }

func WithArtifact(ctx context.Context, session, turn string) context.Context {
	return context.WithValue(ctx, artifactKey{}, ArtifactScope{Session: session, Turn: turn, Capture: uuid.NewString(), transport: &artifactTransportClaim{}})
}

func Artifact(ctx context.Context) (ArtifactScope, bool) {
	scope, ok := ctx.Value(artifactKey{}).(ArtifactScope)
	return scope, ok
}

// ClaimTransport preserves one summary even if a capture is accidentally reused.
func (s ArtifactScope) ClaimTransport() bool {
	if s.transport == nil {
		return false
	}
	if !s.transport.claimed.CompareAndSwap(false, true) {
		s.transport.unobserved.Store(true)
		return false
	}
	return true
}
func (s ArtifactScope) MarkTransportUnobserved() {
	if s.transport != nil {
		s.transport.unobserved.Store(true)
	}
}
func (s ArtifactScope) TransportObserved() bool {
	return s.transport != nil && !s.transport.unobserved.Load()
}
