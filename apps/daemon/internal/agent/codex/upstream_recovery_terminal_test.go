package codex

import (
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func TestAttachRecoveriesRecordsInFlightRecoveryOnce(t *testing.T) {
	s := &Session{}
	s.recovery.snapshot.Store(&recoverySnapshot{oldThreadID: "old", fingerprint: "fp"})
	for i := 0; i < 2; i++ {
		meta := map[string]any{}
		s.recordInflightRecoveryAbort()
		s.attachRecoveries(meta, true)
		got, _ := meta[proto.DoneMetaUpstreamRecoveries].([]proto.UpstreamRecovery)
		if len(got) != 1 || got[0].Outcome != proto.RecoveryOutcomeAborted || got[0].Fingerprint != "fp" || got[0].OldNativeSessionID != "old" {
			t.Fatalf("attempt %d: records = %+v", i, got)
		}
	}
}

func TestAttachRecoveriesSkipsSnapshotAlreadyRecorded(t *testing.T) {
	s := &Session{}
	s.recovery.snapshot.Store(&recoverySnapshot{recorded: true})
	meta := map[string]any{}
	s.recordInflightRecoveryAbort()
	s.attachRecoveries(meta, false)
	if _, ok := meta[proto.DoneMetaUpstreamRecoveries]; ok {
		t.Fatalf("unexpected records: %+v", meta)
	}
}
