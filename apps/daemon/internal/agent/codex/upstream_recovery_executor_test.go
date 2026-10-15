package codex

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// The whole production path with a scripted native process: the controls Core
// sent at preparation reach the Turn, the real history reader reads the poisoned
// thread, the Turn finishes on a rebuilt thread, and the next Turn resumes it.
// The process is a fixture; the live app-server behavior is a V1-V3/V9/V11
// ASSUMPTION to be calibrated against codex 0.153.4.
func TestExecutorRecoversTheTurnAndNextTurnUsesTheRebuiltThread(t *testing.T) {
	req, cfg, root := preparationFixture(t)
	req.ExecutionControls.UpstreamRecovery = &proto.UpstreamRecoveryControls{Enabled: true, Classes: []string{proto.RecoveryClassReasoningState}}
	e, err := testExecutor(t, "recover-400", req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, id := range []string{"first", "second"} {
		out := make(chan proto.Envelope, 64)
		turn, err := e.StartTurn(t.Context(), id, proto.TextInput("hold on and continue"), out)
		if err != nil {
			t.Fatal(err)
		}
		frames := settledFrames(t, turn, out)
		result := summarize(t, frames)
		if result.dones != 1 || len(result.errors) != 0 {
			t.Fatalf("%s: terminal = %+v", id, result)
		}
		sessions = append(sessions, result.done.Metadata[proto.DoneMetaAgentSessionID].(string))
		if id == "first" {
			want := []proto.UpstreamRecovery{{Fingerprint: proto.FingerprintReasoningEncryptedUnverifiable, Action: proto.RecoveryActionRebuildThread, Attempt: 1,
				Outcome: proto.RecoveryOutcomeRecovered, Dropped: proto.RecoveryDropped{ReasoningItems: 1}, OldNativeSessionID: "fixture-native-thread", NewNativeSessionID: "fixture-recovered-thread"}}
			if !reflect.DeepEqual(result.recovery, want) {
				t.Fatalf("records = %+v", result.recovery)
			}
		} else if len(result.recovery) != 0 {
			t.Fatal("the next Turn inherited the previous Turn's recovery records")
		}
		if e.active.recovery.attemptCount() > 1 {
			t.Fatal("attempts are per Turn")
		}
	}
	if !reflect.DeepEqual(sessions, []string{"fixture-recovered-thread", "fixture-recovered-thread"}) {
		t.Fatalf("sessions = %v", sessions)
	}
	var methods []string
	threads := map[string]bool{}
	for _, f := range preparationFrames(t, root) {
		switch f.Method {
		case "thread/start", "thread/read", "turn/start":
			methods = append(methods, f.Method)
			var params struct {
				ThreadID string `json:"threadId"`
			}
			if json.Unmarshal(f.Params, &params) == nil && f.Method == "turn/start" {
				threads[params.ThreadID] = true
			}
		}
	}
	// start, turn, then the recovery (read, start, turn), then the next Turn's turn.
	want := []string{"thread/start", "turn/start", "thread/read", "thread/start", "turn/start", "turn/start"}
	if !reflect.DeepEqual(methods, want) || len(threads) != 2 {
		t.Fatalf("native requests = %v threads = %v", methods, threads)
	}
}

// Without the controls nothing about the Turn changes: the failure is final.
func TestExecutorWithoutRecoveryControlsFailsTheTurn(t *testing.T) {
	req, cfg, root := preparationFixture(t)
	e, err := testExecutor(t, "recover-400", req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan proto.Envelope, 64)
	turn, err := e.StartTurn(t.Context(), "run", proto.TextInput("hold on and continue"), out)
	if err != nil {
		t.Fatal(err)
	}
	result := summarize(t, settledFrames(t, turn, out))
	if result.dones != 1 || len(result.errors) != 1 || result.errors[0].Fingerprint != proto.FingerprintReasoningEncryptedUnverifiable {
		t.Fatalf("terminal = %+v", result)
	}
	if result.done.Metadata[proto.DoneMetaAgentSessionID] != "fixture-native-thread" {
		t.Fatal("the session id changed")
	}
	for _, f := range preparationFrames(t, root) {
		if f.Method == "thread/read" {
			t.Fatal("history was read without recovery")
		}
	}
}
