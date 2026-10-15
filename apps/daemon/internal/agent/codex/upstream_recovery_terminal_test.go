package codex

import (
	"context"
	"encoding/json"
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

func TestEndRecoveryRecordsAbortWhenTerminalWonRace(t *testing.T) {
	s := &Session{}
	s.recovery.snapshot.Store(&recoverySnapshot{oldThreadID: "old", fingerprint: "fp"})
	s.terminal.Store(true)
	s.endRecovery()
	s.recordInflightRecoveryAbort()
	meta := map[string]any{}
	s.attachRecoveries(meta, true)
	got, _ := meta[proto.DoneMetaUpstreamRecoveries].([]proto.UpstreamRecovery)
	if len(got) != 1 || got[0].Outcome != proto.RecoveryOutcomeAborted || got[0].OldNativeSessionID != "old" {
		t.Fatalf("records = %+v", got)
	}
}

func newAckTestSession(t *testing.T) (*Session, *recoveryRun) {
	t.Helper()
	out := make(chan proto.Envelope, 16)
	s := &Session{runID: "run", out: out, cancelCtx: context.Background(), cfg: defaultSessionConfig(testStateRoot(t)), rpc: NewJSONRPCClient(JSONRPCConfig{})}
	s.threadID, s.reportedID = "new", "old"
	snap := &recoverySnapshot{oldThreadID: "old", fingerprint: "fp"}
	s.recovery.snapshot.Store(snap)
	s.recovery.state.Store(recoveryRecovering)
	s.steering.recovering = true
	run := &recoveryRun{fingerprint: "fp", snap: snap, newThreadID: "new"}
	return s, run
}

func TestAcknowledgeRecoveryCommitsIdentityWithRecord(t *testing.T) {
	s, run := newAckTestSession(t)
	if err := s.acknowledgeRecovery(json.RawMessage(`{"turn":{"id":"turn-new"}}`), run); err != nil {
		t.Fatal(err)
	}
	if got := s.reportedThread(); got != "new" {
		t.Fatalf("reported thread = %q", got)
	}
	meta := map[string]any{}
	s.attachRecoveries(meta, false)
	got, _ := meta[proto.DoneMetaUpstreamRecoveries].([]proto.UpstreamRecovery)
	if len(got) != 1 || got[0].Outcome != proto.RecoveryOutcomeRecovered || got[0].OldNativeSessionID != "old" || got[0].NewNativeSessionID != "new" {
		t.Fatalf("records = %+v", got)
	}
}

func TestAcknowledgeRecoveryKeepsAbortedIdentityWhenAbortWasRecorded(t *testing.T) {
	s, run := newAckTestSession(t)
	s.recordInflightRecoveryAbort()
	if err := s.acknowledgeRecovery(json.RawMessage(`{"turn":{"id":"turn-new"}}`), run); err != nil {
		t.Fatal(err)
	}
	if got := s.reportedThread(); got != "old" {
		t.Fatalf("new identity published despite recorded abort: %q", got)
	}
	meta := map[string]any{}
	s.attachRecoveries(meta, false)
	got, _ := meta[proto.DoneMetaUpstreamRecoveries].([]proto.UpstreamRecovery)
	if len(got) != 1 || got[0].Outcome != proto.RecoveryOutcomeAborted || got[0].OldNativeSessionID != "old" {
		t.Fatalf("records = %+v", got)
	}
}
