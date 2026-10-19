package textobs

import (
	"context"
	"testing"
)

func TestArtifactScopeIsCaptureLocal(t *testing.T) {
	if _, ok := Artifact(context.Background()); ok {
		t.Fatal("implicit scope")
	}
	first := WithArtifact(t.Context(), "session", "turn")
	second := WithArtifact(first, "session", "turn")
	a, _ := Artifact(first)
	b, _ := Artifact(second)
	if a.Session != "session" || a.Turn != "turn" || a.Capture == "" || a.Capture == b.Capture {
		t.Fatal("capture identity not isolated")
	}
	child, cancel := context.WithCancel(first)
	cancel()
	if got, ok := Artifact(child); !ok || got != a {
		t.Fatal("cancellation lost immutable identity")
	}
}
