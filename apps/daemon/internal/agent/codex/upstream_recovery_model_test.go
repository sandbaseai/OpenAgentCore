package codex

import "testing"

func TestRestoreFromSnapshotRestoresResolvedModel(t *testing.T) {
	s := &Session{resolvedModel: "new-model"}
	s.restoreFromSnapshot(&recoverySnapshot{oldThreadID: "old", resolvedModel: "old-model"})
	if s.resolvedModel != "old-model" {
		t.Fatalf("resolvedModel = %q", s.resolvedModel)
	}
}
