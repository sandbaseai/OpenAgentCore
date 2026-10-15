package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

const (
	usageFailed = `{"inputTokens":100,"outputTokens":20,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":120}`
	usageNew    = `{"inputTokens":30,"outputTokens":5,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":35}`
)

// finishNewTurn plays the rebuilt Turn's completion.
func (f *recoveryFixture) finishNewTurn(text string) {
	f.t.Helper()
	f.s.appendFinalText(text)
	raw, _ := json.Marshal(map[string]any{"threadId": newThread, "turn": map[string]any{"id": newTurn, "status": "completed", "usage": json.RawMessage(usageNew)}})
	f.native.notify("turn/completed", json.RawMessage(raw))
}

func turnStartInput(t *testing.T, f *recoveryFixture) string {
	t.Helper()
	req, ok := f.native.request("turn/start")
	if !ok {
		t.Fatal("no turn/start was sent")
	}
	var params TurnStartParams
	raw, _ := json.Marshal(req.Params)
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	if params.ThreadID != newThread {
		t.Fatalf("turn/start thread = %q", params.ThreadID)
	}
	var text strings.Builder
	for _, in := range params.Input {
		text.WriteString(in.Text)
		if in.Type != UserInputText {
			text.WriteString("<" + string(in.Type) + ">")
		}
	}
	return text.String()
}

func TestRecoveryRebuildsThreadAndCompletesTheSameTurn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fingerprint string
		message     string
		history     string
		class       string
		want        []string
		dropped     proto.RecoveryDropped
	}{
		{"B reasoning", proto.FingerprintReasoningEncryptedUnverifiable, msgReasoning, "reasoning", proto.RecoveryClassReasoningState,
			[]string{"[user]\nbuild the thing", "[assistant]\nbuilt it", "[tool call] command (exit 0) (cwd: /work): ls", "[tool output, untrusted]\na.txt\nb.txt", "[assistant]\nlisted files"}, proto.RecoveryDropped{ReasoningItems: 2}},
		{"C history image", proto.FingerprintModelNoImageInput, msgNoImage, "history-image", proto.RecoveryClassImageInput,
			[]string{"[user]\nlook at this\n[image omitted: user_image]"}, proto.RecoveryDropped{Images: 1}},
		{"D tool result image", proto.FingerprintToolResultInputImageRejected, msgToolImage, "tool-image", proto.RecoveryClassToolResultImage,
			[]string{"[tool call] tool: screenshot", "[image omitted: tool_image]"}, proto.RecoveryDropped{ReasoningItems: 1, Images: 1}},
		{"E trailing assistant", proto.FingerprintTrailingModelTurn, msgTrailing, "trailing", proto.RecoveryClassTrailingAssistant,
			[]string{"[assistant]\npartial answer"}, proto.RecoveryDropped{ReasoningItems: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(tc.class), history: fixtureHistory(tc.history)})
			f.s.appendFinalText("streamed before the failure")
			f.fail(tc.message, usageFailed)
			f.eventually("the new Turn to be acknowledged", f.acked)
			if got := f.native.methods(); !reflect.DeepEqual(got, []string{"thread/start", "turn/start"}) {
				t.Fatalf("requests = %v", got)
			}
			input := turnStartInput(t, f)
			for _, want := range append(tc.want, "<recovered_history>", "</recovered_history>", "please continue", recoveryPreambleMarker, recoveryContinueMarker) {
				if !strings.Contains(input, want) {
					t.Fatalf("recovery input is missing %q:\n%s", want, input)
				}
			}
			// The Turn's own input is sent once, after the transcript.
			if strings.Count(input, "please continue") != 1 || strings.Index(input, "</recovered_history>") > strings.Index(input, "please continue") {
				t.Fatalf("current input is not sent exactly once after the history:\n%s", input)
			}
			for _, secret := range []string{canaryReasoning, canaryImageData, "CANARY_IMAGE_BYTES"} {
				if strings.Contains(input, secret) {
					t.Fatalf("recovery input carries %q", secret)
				}
			}
			if !strings.HasSuffix(strings.TrimSpace(input), "Continue working on the current request above.") {
				t.Fatal("recovery input does not end with user content")
			}
			// The thread parameters are rebuilt from the same plan as the first start.
			req, _ := f.native.request("thread/start")
			raw, _ := json.Marshal(req.Params)
			var start ThreadStartParams
			_ = json.Unmarshal(raw, &start)
			if start.Cwd != "/work" || start.Model != "synthetic" || start.DeveloperInstructions != "be useful" || start.ApprovalPolicy != approvalPolicyNever || start.Sandbox != sandboxDangerFullAccess {
				t.Fatalf("thread/start = %+v", start)
			}
			// Nothing has been settled yet: the Turn continues on the new thread.
			if f.s.terminal.Load() || f.s.reportedThread() != newThread || f.s.currentThreadID() != newThread {
				t.Fatalf("terminal=%t reported=%s current=%s", f.s.terminal.Load(), f.s.reportedThread(), f.s.currentThreadID())
			}
			f.finishNewTurn("all done")
			result := summarize(t, f.frames())
			if result.dones != 1 || len(result.errors) != 0 || result.done.Content != "streamed before the failure\n\nall done" {
				t.Fatalf("terminal = %+v", result)
			}
			if result.done.Metadata[proto.DoneMetaAgentSessionID] != newThread {
				t.Fatalf("agent_session_id = %v", result.done.Metadata[proto.DoneMetaAgentSessionID])
			}
			want := proto.UpstreamRecovery{Fingerprint: tc.fingerprint, Action: proto.RecoveryActionRebuildThread, Attempt: 1, Outcome: proto.RecoveryOutcomeRecovered,
				Dropped: tc.dropped, OldNativeSessionID: oldThread, NewNativeSessionID: newThread}
			if !reflect.DeepEqual(result.recovery, []proto.UpstreamRecovery{want}) {
				t.Fatalf("records = %+v", result.recovery)
			}
			// The failed Turn's usage and the rebuilt Turn's add up, published as one total.
			if result.done.Usage.InputTokens != 130 || result.done.Usage.OutputTokens != 25 {
				t.Fatalf("usage = %+v", result.done.Usage)
			}
			for _, u := range result.usages {
				if u.InputTokens != 130 {
					t.Fatalf("a non-cumulative usage was published: %+v", result.usages)
				}
			}
			// The next Turn resumes the rebuilt thread.
			if f.s.reportedThread() != newThread {
				t.Fatal("next Turn would resume the poisoned thread")
			}
		})
	}
}

func TestRecoveryDoesNotApply(t *testing.T) {
	allClasses := enabledControls(proto.UpstreamRecoveryClasses()...)
	for _, tc := range []struct {
		name     string
		controls proto.UpstreamRecoveryControls
		message  string
	}{
		{"A is only classified", allClasses, msgMessages},
		{"a 400 outside the whitelist", allClasses, msgPlain400},
		{"not a 400", allClasses, "unexpected status 429 Too Many Requests: slow down"},
		{"D is off by default", proto.UpstreamRecoveryControls{Enabled: true}, msgToolImage},
		{"E is off by default", proto.UpstreamRecoveryControls{Enabled: true}, msgTrailing},
		{"B is outside the allowed classes", enabledControls(proto.RecoveryClassImageInput), msgReasoning},
		{"disabled", proto.UpstreamRecoveryControls{Enabled: false, Classes: proto.UpstreamRecoveryClasses()}, msgReasoning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t, fixtureOptions{controls: tc.controls, history: fixtureHistory("reasoning")})
			f.fail(tc.message, usageFailed)
			result := summarize(t, f.frames())
			if result.dones != 1 || len(result.errors) != 1 {
				t.Fatalf("terminal = %+v", result)
			}
			if got := f.native.methods(); len(got) != 0 || f.reader.calls.Load() != 0 {
				t.Fatalf("a recovery was attempted: %v", got)
			}
			if _, present := result.done.Metadata[proto.DoneMetaUpstreamRecoveries]; present {
				t.Fatal("recovery records without a recovery")
			}
			if result.done.Metadata[proto.DoneMetaAgentSessionID] != oldThread {
				t.Fatal("the session id changed")
			}
		})
	}
}

// Without recovery the terminal output is exactly what the abandoned recovery
// produces, apart from the aborted record it reports.
func TestAbandonedRecoveryMatchesTheRunWithoutRecovery(t *testing.T) {
	baseline := func() comparableTerminal {
		f := newRecoveryFixture(t, fixtureOptions{controls: proto.UpstreamRecoveryControls{}, history: fixtureHistory("reasoning")})
		f.s.appendFinalText("streamed before the failure")
		f.fail(msgReasoning, usageFailed)
		return summarize(t, f.frames()).comparable()
	}()
	if baseline.Dones != 1 || len(baseline.Errors) != 1 || baseline.Usage.InputTokens != 100 || baseline.Session != oldThread {
		t.Fatalf("baseline = %+v", baseline)
	}
	hold := func(method string) func(f *recoveryFixture) {
		return func(f *recoveryFixture) { f.native.hold[method] = nil }
	}
	for _, tc := range []struct {
		name   string
		reason string
		setup  func(f *recoveryFixture)
	}{
		{"history unreadable", proto.RecoveryAbortHistoryUnreadable, func(f *recoveryFixture) { f.reader.err = context.DeadlineExceeded }},
		{"history has nothing to drop", proto.RecoveryAbortNotApplicable, func(f *recoveryFixture) {
			f.reader.turns = []historyTurn{{ID: oldTurn, Status: "failed", Items: []json.RawMessage{userItem(text("request"))}}}
		}},
		{"thread/start refused", proto.RecoveryAbortThreadStartFailed, func(f *recoveryFixture) { f.native.fail["thread/start"] = "no capacity" }},
		{"thread/start timeout", proto.RecoveryAbortThreadStartFailed, hold("thread/start")},
		{"turn/start refused", proto.RecoveryAbortTurnStartFailed, func(f *recoveryFixture) { f.native.fail["turn/start"] = "rejected" }},
		{"turn/start timeout", proto.RecoveryAbortTurnStartFailed, hold("turn/start")},
	} {
		name := tc.name
		t.Run(name, func(t *testing.T) {
			f := newRecoveryFixture(t, fixtureOptions{
				controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"),
				timeouts: recoveryTimeouts{Total: 5 * time.Second, History: 200 * time.Millisecond, ThreadStart: 200 * time.Millisecond, TurnStart: 200 * time.Millisecond},
			})
			tc.setup(f)
			f.s.appendFinalText("streamed before the failure")
			f.fail(msgReasoning, usageFailed)
			result := summarize(t, f.frames())
			if got := result.comparable(); !reflect.DeepEqual(got, baseline) {
				t.Fatalf("abandoned recovery differs from the run without recovery:\n got  %+v\n want %+v", got, baseline)
			}
			if len(result.recovery) != 1 || result.recovery[0].Outcome != proto.RecoveryOutcomeAborted || result.recovery[0].Reason != tc.reason || result.recovery[0].Attempt != 0 {
				t.Fatalf("records = %+v, want one aborted/%s", result.recovery, tc.reason)
			}
			if f.s.currentThreadID() != oldThread || f.s.reportedThread() != oldThread {
				t.Fatalf("routing was not restored: %s/%s", f.s.currentThreadID(), f.s.reportedThread())
			}
			f.s.usageMu.Lock()
			clean := f.s.usageCarry == nil && !f.s.usageInRecovery && !f.s.usageSwitched
			f.s.usageMu.Unlock()
			if !clean {
				t.Fatal("usage recovery state was not restored")
			}
		})
	}
}

func TestRecoveryThreadUsedCoversEveryEarlierThread(t *testing.T) {
	s := &Session{}
	s.recovery.records = []proto.UpstreamRecovery{
		{OldNativeSessionID: "thread-a", NewNativeSessionID: "thread-b"},
		{OldNativeSessionID: "thread-b", NewNativeSessionID: "thread-c"},
	}
	for _, id := range []string{"thread-a", "thread-b", "thread-c"} {
		if !s.recoveryThreadUsed(id) {
			t.Fatalf("%s must be rejected as a recovery target", id)
		}
	}
	if s.recoveryThreadUsed("thread-d") {
		t.Fatal("a fresh thread must be accepted")
	}
}
