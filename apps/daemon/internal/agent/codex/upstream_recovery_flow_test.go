package codex

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func (f *recoveryFixture) failNew(thread, turn, message, usage string) {
	f.t.Helper()
	f.native.notify("turn/completed", failedTurnJSON(thread, turn, message, usage))
}

func (f *recoveryFixture) settled() agent.TurnSettlement {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settlement, err := f.s.AwaitSettlement(ctx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		f.t.Logf("settlement error: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		f.t.Fatal("Turn did not settle")
	}
	return settlement
}

func (f *recoveryFixture) cancelTurn() chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- f.s.Cancel(ctx)
	}()
	return done
}

func (f *recoveryFixture) seen(method string) func() bool {
	return func() bool { return f.native.count(method) > 0 }
}

func TestRecoveryAttemptLimits(t *testing.T) {
	t.Run("one attempt per Turn", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState, proto.RecoveryClassImageInput), history: fixtureHistory("both")})
		f.fail(msgReasoning, usageFailed)
		f.eventually("first rebuild", f.acked)
		// A different recoverable failure of the rebuilt Turn is out of attempts.
		f.failNew(newThread, newTurn, msgNoImage, usageNew)
		result := summarize(t, f.frames())
		if f.native.count("thread/start") != 1 || result.dones != 1 || len(result.errors) != 1 {
			t.Fatalf("thread/start=%d terminal=%+v", f.native.count("thread/start"), result)
		}
		if result.done.Metadata[proto.DoneMetaAgentSessionID] != newThread {
			t.Fatal("a failure after the switch must report the new thread")
		}
		if len(result.recovery) != 1 || result.recovery[0].Outcome != proto.RecoveryOutcomeExhausted || result.recovery[0].Attempt != 1 {
			t.Fatalf("records = %+v", result.recovery)
		}
	})
	t.Run("two attempts allow a different fingerprint once", func(t *testing.T) {
		controls := enabledControls(proto.RecoveryClassReasoningState, proto.RecoveryClassImageInput)
		controls.MaxAttempts = 2
		f := newRecoveryFixture(t, fixtureOptions{controls: controls, history: fixtureHistory("both")})
		f.fail(msgReasoning, usageFailed)
		f.eventually("first rebuild", f.acked)
		f.failNew(newThread, newTurn, msgNoImage, usageNew)
		f.eventually("second rebuild", func() bool { return f.s.recovery.attemptCount() == 2 && f.s.recovery.state.Load() == recoveryIdle })
		// The same fingerprint again is never retried within a Turn.
		f.failNew(secondThread, secondTurn, msgNoImage, usageNew)
		result := summarize(t, f.frames())
		if f.native.count("thread/start") != 2 || result.dones != 1 || len(result.errors) != 1 {
			t.Fatalf("thread/start=%d terminal=%+v", f.native.count("thread/start"), result)
		}
		if result.done.Metadata[proto.DoneMetaAgentSessionID] != secondThread {
			t.Fatal("session id is not the last rebuilt thread")
		}
		want := []proto.UpstreamRecovery{
			{Fingerprint: proto.FingerprintReasoningEncryptedUnverifiable, Action: proto.RecoveryActionRebuildThread, Attempt: 1, Outcome: proto.RecoveryOutcomeExhausted, Dropped: proto.RecoveryDropped{ReasoningItems: 1, Images: 1}, OldNativeSessionID: oldThread, NewNativeSessionID: newThread},
			{Fingerprint: proto.FingerprintModelNoImageInput, Action: proto.RecoveryActionRebuildThread, Attempt: 2, Outcome: proto.RecoveryOutcomeExhausted, Dropped: proto.RecoveryDropped{ReasoningItems: 1, Images: 1}, OldNativeSessionID: newThread, NewNativeSessionID: secondThread},
		}
		if !reflect.DeepEqual(result.recovery, want) {
			t.Fatalf("records = %+v", result.recovery)
		}
		// 100 + 30 + 30: each failed Turn's usage counts once.
		if result.done.Usage.InputTokens != 160 {
			t.Fatalf("usage = %+v", result.done.Usage)
		}
	})
	t.Run("an abandoned second attempt counts the failed Turn once", func(t *testing.T) {
		controls := enabledControls(proto.RecoveryClassReasoningState, proto.RecoveryClassImageInput)
		controls.MaxAttempts = 2
		f := newRecoveryFixture(t, fixtureOptions{controls: controls, history: fixtureHistory("both")})
		f.reader.failFrom = 1
		f.fail(msgReasoning, usageFailed)
		f.eventually("first rebuild", f.acked)
		f.failNew(newThread, newTurn, msgNoImage, usageNew)
		result := summarize(t, f.frames())
		if result.done.Usage.InputTokens != 130 || result.done.Usage.OutputTokens != 25 {
			t.Fatalf("usage = %+v, want the two failed Turns once each", result.done.Usage)
		}
		if len(result.recovery) != 2 || result.recovery[0].Outcome != proto.RecoveryOutcomeExhausted || result.recovery[1].Outcome != proto.RecoveryOutcomeAborted ||
			result.recovery[1].Reason != proto.RecoveryAbortHistoryUnreadable || result.recovery[1].Attempt != 0 {
			t.Fatalf("records = %+v", result.recovery)
		}
		if result.done.Metadata[proto.DoneMetaAgentSessionID] != newThread {
			t.Fatal("an abandoned second attempt keeps the thread of the first")
		}
	})
}

func TestRecoveryTimeBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(f *recoveryFixture)
		reason string
	}{
		{"reading history", func(f *recoveryFixture) { f.reader.block = make(chan struct{}) }, proto.RecoveryAbortHistoryUnreadable},
		{"thread/start", func(f *recoveryFixture) { f.native.hold["thread/start"] = nil }, proto.RecoveryAbortThreadStartFailed},
		{"turn/start", func(f *recoveryFixture) { f.native.hold["turn/start"] = nil }, proto.RecoveryAbortTurnStartFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t, fixtureOptions{
				controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true,
				timeouts: recoveryTimeouts{Total: 5 * time.Second, History: 100 * time.Millisecond, ThreadStart: 100 * time.Millisecond, TurnStart: 100 * time.Millisecond},
			})
			tc.setup(f)
			started := time.Now()
			f.fail(msgReasoning, usageFailed)
			result := summarize(t, f.frames())
			if time.Since(started) > 3*time.Second {
				t.Fatal("the step was not bounded")
			}
			if len(result.recovery) != 1 || result.recovery[0].Reason != tc.reason || len(result.errors) != 1 {
				t.Fatalf("terminal = %+v", result)
			}
			settlement := f.settled()
			// An unanswered turn/start may have started natively: never reuse it.
			if (tc.reason == proto.RecoveryAbortTurnStartFailed) == settlement.Reusable {
				t.Fatalf("reusable = %t after %s", settlement.Reusable, tc.name)
			}
		})
	}
	t.Run("the whole recovery is bounded", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{
			controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"),
			timeouts: recoveryTimeouts{Total: 150 * time.Millisecond, History: time.Minute, ThreadStart: time.Minute, TurnStart: time.Minute},
		})
		f.reader.block = make(chan struct{})
		f.fail(msgReasoning, usageFailed)
		result := summarize(t, f.frames())
		if len(result.recovery) != 1 || result.recovery[0].Reason != proto.RecoveryAbortHistoryUnreadable {
			t.Fatalf("records = %+v", result.recovery)
		}
	})
}

func TestRecoveryCancellationWins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(f *recoveryFixture)
		window   func(f *recoveryFixture) func() bool
		reusable bool
	}{
		{"while reading history", func(f *recoveryFixture) { f.reader.block = make(chan struct{}) },
			func(f *recoveryFixture) func() bool { return func() bool { return f.reader.calls.Load() > 0 } }, true},
		{"while starting the thread", func(f *recoveryFixture) { f.native.hold["thread/start"] = nil },
			func(f *recoveryFixture) func() bool { return f.seen("thread/start") }, true},
		{"while the new turn is unanswered", func(f *recoveryFixture) { f.native.hold["turn/start"] = nil },
			func(f *recoveryFixture) func() bool { return f.seen("turn/start") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
			tc.setup(f)
			f.fail(msgReasoning, usageFailed)
			f.eventually(tc.name, tc.window(f))
			if err := <-f.cancelTurn(); err != nil {
				t.Logf("cancel: %v", err)
			}
			result := summarize(t, f.frames())
			if result.dones != 1 {
				t.Fatalf("terminals = %d", result.dones)
			}
			if len(result.recovery) != 1 || result.recovery[0].Reason != proto.RecoveryAbortCancelled || result.recovery[0].Outcome != proto.RecoveryOutcomeAborted {
				t.Fatalf("records = %+v", result.recovery)
			}
			if result.done.Metadata[proto.DoneMetaAgentSessionID] != oldThread {
				t.Fatal("a cancelled recovery reports the old thread")
			}
			// Settlement waits for the recovery goroutine, so reaching it proves it ended.
			if settlement := f.settled(); settlement.Reusable != tc.reusable {
				t.Fatalf("reusable = %t", settlement.Reusable)
			}
			if f.native.count("turn/interrupt") != 0 {
				t.Fatal("the already terminal old Turn was interrupted")
			}
		})
	}
	t.Run("after the new turn was acknowledged", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
		f.fail(msgReasoning, usageFailed)
		f.eventually("the new Turn to be acknowledged", f.acked)
		cancelled := f.cancelTurn()
		f.eventually("the interrupt", f.seen("turn/interrupt"))
		interrupt, _ := f.native.request("turn/interrupt")
		raw, _ := json.Marshal(interrupt.Params)
		var params TurnInterruptParams
		_ = json.Unmarshal(raw, &params)
		if params.ThreadID != newThread || params.TurnID != newTurn {
			t.Fatalf("interrupted %+v, want the rebuilt Turn", params)
		}
		f.native.notify("turn/completed", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`","status":"interrupted"}}`))
		if err := <-cancelled; err != nil {
			t.Fatal(err)
		}
		result := summarize(t, f.frames())
		if result.dones != 1 || result.done.Metadata[proto.DoneMetaAgentSessionID] != newThread {
			t.Fatalf("terminal = %+v", result)
		}
	})
}

func TestSteerDuringRecovery(t *testing.T) {
	steer := func(f *recoveryFixture, written func()) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		input := proto.PromptSteerPayload{InputID: "input", Input: proto.TextInput("more")}
		if written != nil {
			return f.s.SteerWithReceipt(ctx, input, written)
		}
		return f.s.Steer(ctx, input)
	}
	t.Run("a steer while recovering is not ready and is sent nowhere", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
		f.native.hold["thread/start"] = nil
		f.fail(msgReasoning, usageFailed)
		f.eventually("thread/start", f.seen("thread/start"))
		for _, written := range []func(){nil, func() {}} {
			if err := steer(f, written); !errors.Is(err, agent.ErrSteeringNotReady) {
				t.Fatalf("steer = %v, want ErrSteeringNotReady (the Turn must not end with input_run_inactive)", err)
			}
		}
		if f.native.count("turn/steer") != 0 {
			t.Fatal("input reached a terminal Turn")
		}
	})
	t.Run("the rebuilt turn accepts input again", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
		f.fail(msgReasoning, usageFailed)
		f.eventually("the new Turn to be acknowledged", f.acked)
		if err := steer(f, nil); err != nil {
			t.Fatal(err)
		}
		req, _ := f.native.request("turn/steer")
		raw, _ := json.Marshal(req.Params)
		var params TurnSteerParams
		_ = json.Unmarshal(raw, &params)
		if params.ThreadID != newThread || params.ExpectedTurnID != newTurn {
			t.Fatalf("steer targeted %+v", params)
		}
	})
	t.Run("a steer in flight at takeover", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			written bool
			fail    string
		}{
			{"never answered", false, ""},
			{"rejected after the takeover", false, "rejected"},
			{"already written", true, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
				release := make(chan struct{})
				f.native.hold["turn/steer"] = release
				if tc.fail != "" {
					f.native.fail["turn/steer"] = tc.fail
				}
				var wrote sync.WaitGroup
				var written func()
				if tc.written {
					wrote.Add(1)
					var once sync.Once
					written = func() { once.Do(wrote.Done) }
				}
				result := make(chan error, 1)
				go func() { result <- steer(f, written) }()
				f.eventually("the steer on the wire", f.seen("turn/steer"))
				if tc.written {
					wrote.Wait()
				}
				f.native.hold["thread/start"] = nil
				f.fail(msgReasoning, usageFailed)
				select {
				case err := <-result:
					// The frame is on the wire, so native may have accepted it:
					// the outcome stays unknown and Core must not send it again.
					if err == nil || errors.Is(err, agent.ErrSteeringNotReady) || errors.Is(err, agent.ErrSteeringInactive) || errors.Is(err, agent.ErrSteeringRejected) {
						t.Fatalf("steer = %v, want an outcome-unknown error", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("the in-flight steer was not closed at takeover")
				}
				f.steeringSlotEmpty()
				close(release)
				if f.native.count("turn/steer") != 1 {
					t.Fatal("the input was sent twice")
				}
				cancelled := f.cancelTurn()
				<-cancelled
				summarize(t, f.frames())
				f.settled() // the steer's operation was released
			})
		}
	})
	t.Run("a second concurrent steer does not replace the cancel slot", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
		f.native.hold["turn/steer"] = nil
		first := make(chan error, 1)
		go func() { first <- steer(f, nil) }()
		f.eventually("the first steer on the wire", f.seen("turn/steer"))
		if err := steer(f, nil); !errors.Is(err, agent.ErrSteeringNotReady) {
			t.Fatalf("second steer = %v", err)
		}
		f.s.steering.mu.Lock()
		held := f.s.steering.cancel != nil
		f.s.steering.mu.Unlock()
		if !held || f.native.count("turn/steer") != 1 {
			t.Fatal("the second steer disturbed the first")
		}
		f.s.stopSteering()
		if err := <-first; err == nil {
			t.Fatal("the first steer survived stopSteering")
		}
	})
}

func (f *recoveryFixture) steeringSlotEmpty() {
	f.t.Helper()
	f.eventually("the steer to release the cancel slot", func() bool {
		f.s.steering.mu.Lock()
		defer f.s.steering.mu.Unlock()
		return f.s.steering.cancel == nil
	})
}

func TestRecoveryUsageAndOutcomeAcrossWindows(t *testing.T) {
	usageUpdate := func(thread, turn string, input, output int) json.RawMessage {
		return json.RawMessage(`{"threadId":"` + thread + `","turnId":"` + turn + `","tokenUsage":{"total":{"inputTokens":` + itoa(input) + `,"outputTokens":` + itoa(output) + `,"totalTokens":` + itoa(input+output) + `}}}`)
	}
	t.Run("usage before takeover is carried, later old traffic is dropped", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
		f.native.hold["thread/start"] = nil
		f.s.onUsageUpdated(usageUpdate(oldThread, oldTurn, 100, 20))
		before := len(f.out)
		f.s.appendFinalText("text before the failure")
		f.fail(msgReasoning, "")
		f.eventually("thread/start", f.seen("thread/start"))
		// (b) old-thread usage after takeover and before the switch is dropped.
		f.s.onUsageUpdated(usageUpdate(oldThread, oldTurn, 500, 90))
		f.s.usageMu.Lock()
		latest, carry := *f.s.latestUsage, *f.s.usageCarry
		f.s.usageMu.Unlock()
		if latest.InputTokens != 100 || carry.InputTokens != 100 || len(f.out) != before {
			t.Fatalf("late old usage leaked: latest=%+v carry=%+v published=%d", latest, carry, len(f.out)-before)
		}
		// (c) cancellation outcome before the switch: the carry alone, the snapshot text.
		outcome := f.s.CancellationOutcome()
		if outcome.Usage.InputTokens != 100 || outcome.Content != "text before the failure" || outcome.Metadata[proto.DoneMetaAgentSessionID] != oldThread {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
	t.Run("after the switch the outcome adds the rebuilt Turn's usage once", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
		release := make(chan struct{})
		f.native.hold["turn/start"] = release
		f.s.appendFinalText("text before the failure")
		f.fail(msgReasoning, usageFailed)
		f.eventually("turn/start", f.seen("turn/start"))
		// (c) switched, the new Turn has produced nothing yet.
		outcome := f.s.CancellationOutcome()
		if outcome.Usage.InputTokens != 100 || outcome.Content != "text before the failure" || outcome.Metadata[proto.DoneMetaAgentSessionID] != oldThread {
			t.Fatalf("outcome = %+v", outcome)
		}
		// (d) the rebuilt Turn reports usage before its start is acknowledged: it is
		// held, so the outcome keeps the failed Turn's usage until the reply commits.
		f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
		f.native.notify("thread/tokenUsage/updated", usageUpdate(newThread, newTurn, 30, 5))
		f.eventually("the notifications to be held", func() bool {
			f.s.steering.mu.Lock()
			defer f.s.steering.mu.Unlock()
			return f.s.steering.gate != nil && len(f.s.steering.gate.queue) == 2
		})
		if got := f.s.CancellationOutcome().Usage.InputTokens; got != 100 {
			t.Fatalf("held usage leaked into the outcome: %d", got)
		}
		close(release)
		f.eventually("the new usage", func() bool {
			return f.s.CancellationOutcome().Usage.InputTokens == 130
		})
		first, second := f.s.CancellationOutcome(), f.s.CancellationOutcome()
		if !reflect.DeepEqual(first, second) || first.Usage.OutputTokens != 25 {
			t.Fatalf("outcome is not stable: %+v / %+v", first, second)
		}
	})
	t.Run("a notification admitted before a rollback cannot write the restored state", func(t *testing.T) {
		f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
		f.native.fail["turn/start"] = "rejected"
		f.native.before["turn/start"] = func(JsonRpcRequest) {
			f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
			f.native.notify("thread/tokenUsage/updated", usageUpdate(newThread, newTurn, 7777, 7))
			f.native.notify("error", json.RawMessage(`{"threadId":"`+newThread+`","turnId":"`+newTurn+`","message":"late `+canarySecret+`"}`))
		}
		f.s.appendFinalText("text before the failure")
		f.fail(msgReasoning, usageFailed)
		result := summarize(t, f.frames())
		if result.done.Usage.InputTokens != 100 || result.dones != 1 || len(result.errors) != 1 {
			t.Fatalf("terminal = %+v", result)
		}
		if strings.Contains(result.errors[0].Error, canarySecret) || !strings.Contains(result.errors[0].Error, "text before the failure") {
			t.Fatalf("error text was not restored: %q", result.errors[0].Error)
		}
		// A turn/started was observed before the refusal, so native may still be
		// running the rebuilt Turn and the process must not be reused.
		if f.settled(); !f.s.recovery.unconfirmed.Load() {
			t.Fatal("an observed start must leave the native outcome unconfirmed")
		}
	})
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestRecoveryToleratesNotificationsBeforeTheReply(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
	f.native.before["turn/start"] = func(JsonRpcRequest) {
		// The rebuilt Turn starts and streams before the RPC reply arrives.
		f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
		f.native.notify("item/agentMessage/delta", json.RawMessage(`{"threadId":"`+newThread+`","turnId":"`+newTurn+`","itemId":"m1","delta":"hello"}`))
	}
	f.s.onAgentDelta(json.RawMessage(`{"threadId":"` + oldThread + `","turnId":"` + oldTurn + `","itemId":"m0","delta":"before"}`))
	f.fail(msgReasoning, usageFailed)
	f.eventually("the new Turn to be acknowledged", f.acked)
	// A repeated terminal notification for the failed Turn is dropped.
	f.s.onTurnCompleted(failedTurnJSON(oldThread, oldTurn, msgReasoning, usageFailed))
	f.finishNewTurn("done")
	result := summarize(t, f.frames())
	if f.native.count("thread/start") != 1 || result.dones != 1 || len(result.errors) != 0 {
		t.Fatalf("thread/start=%d terminal=%+v", f.native.count("thread/start"), result)
	}
	var sequences []uint64
	for _, env := range result.raw {
		if env.Type == proto.TypeDelta {
			var p proto.DeltaPayload
			_ = env.DecodePayload(&p)
			sequences = append(sequences, p.Sequence)
		}
	}
	if !reflect.DeepEqual(sequences, []uint64{1, 2}) {
		t.Fatalf("delta sequences = %v, want a continuing sequence across the rebuild", sequences)
	}
	if !f.settled().Reusable {
		t.Fatal("a recovered Turn leaves a reusable process")
	}
}

func TestRecoveryDoesNotBlockCancellationOutcomeOrSettlement(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
	f.reader.block = make(chan struct{})
	f.fail(msgReasoning, usageFailed)
	f.eventually("the history read", func() bool { return f.reader.calls.Load() > 0 })
	read := make(chan proto.DonePayload, 1)
	go func() { read <- f.s.CancellationOutcome() }()
	select {
	case outcome := <-read:
		if outcome.Metadata[proto.DoneMetaAgentSessionID] != oldThread {
			t.Fatalf("outcome = %+v", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancellationOutcome is blocked by the recovery")
	}
	time.Sleep(50 * time.Millisecond)
	f.s.operationMu.Lock()
	closed := f.s.operationsClosed
	f.s.operationMu.Unlock()
	select {
	case <-f.s.waitDone:
		t.Fatal("the Turn settled while its recovery was running")
	default:
	}
	if closed {
		t.Fatal("the Turn's handlers were detached while its recovery was running")
	}
	close(f.reader.block)
	f.eventually("the new Turn to be acknowledged", f.acked)
	f.finishNewTurn("done")
	summarize(t, f.frames())
	f.settled()
}

func TestShouldRecoverGuards(t *testing.T) {
	base := func() *recoveryFixture {
		return newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
	}
	failedTurn := TurnCompletedNotification{ThreadID: oldThread, Turn: Turn{ID: oldTurn, Status: "failed", Error: &TurnError{Message: msgReasoning}}}
	if _, ok := base().s.shouldRecover(failedTurn); !ok {
		t.Fatal("a recoverable failure was not recovered")
	}
	for name, mutate := range map[string]func(f *recoveryFixture){
		"already recovering": func(f *recoveryFixture) { f.s.recovery.state.Store(recoveryRecovering) },
		"cancelled":          func(f *recoveryFixture) { f.s.cancelled.Store(true) },
		"turn context done":  func(f *recoveryFixture) { f.s.cancelFn() },
		"terminal":           func(f *recoveryFixture) { f.s.terminal.Store(true) },
		"attempts used":      func(f *recoveryFixture) { f.s.recovery.attempts = 1 },
		"fingerprint used": func(f *recoveryFixture) {
			f.s.recovery.used = map[string]bool{proto.FingerprintReasoningEncryptedUnverifiable: true}
		},
		"subagents":   func(f *recoveryFixture) { f.s.subagents = &subagentObservations{} },
		"no plan":     func(f *recoveryFixture) { f.s.recovery.plan = nil },
		"no executor": func(f *recoveryFixture) { f.s.executor = nil },
	} {
		f := base()
		mutate(f)
		if _, ok := f.s.shouldRecover(failedTurn); ok {
			t.Fatalf("%s: recovery was allowed", name)
		}
	}
	completed := failedTurn
	completed.Turn.Status = "completed"
	if _, ok := base().s.shouldRecover(completed); ok {
		t.Fatal("a completed Turn was recovered")
	}
}

func TestEffectiveUsageStates(t *testing.T) {
	one := &TurnUsage{observed: true, complete: true, InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	two := &TurnUsage{observed: true, complete: true, InputTokens: 5, OutputTokens: 1, TotalTokens: 6}
	partial := &TurnUsage{observed: true, InputTokens: 1}
	for _, tc := range []struct {
		name                       string
		carry, latest              *TurnUsage
		inRecovery, switched       bool
		wantInput                  int
		wantNil, wantCompleteTotal bool
	}{
		{"no recovery, no carry", nil, one, false, false, 10, false, true},
		{"no usage at all", nil, nil, false, false, 0, true, false},
		{"settled after a recovery adds both", one, two, false, false, 15, false, true},
		{"before the switch only the carry counts", one, two, true, false, 10, false, true},
		{"after the switch both count", one, two, true, true, 15, false, true},
		{"after the switch with nothing new", one, nil, true, true, 10, false, true},
		{"incomplete input stays incomplete", one, partial, false, false, 11, false, false},
	} {
		s := &Session{usageCarry: tc.carry, latestUsage: tc.latest, usageInRecovery: tc.inRecovery, usageSwitched: tc.switched}
		got := s.effectiveUsageLocked()
		if tc.wantNil != (got == nil) {
			t.Fatalf("%s: got %+v", tc.name, got)
		}
		if got != nil && (got.InputTokens != tc.wantInput || got.complete != tc.wantCompleteTotal) {
			t.Fatalf("%s: got %+v", tc.name, got)
		}
	}
	if (&Session{latestUsage: one}).effectiveUsageLocked() != one {
		t.Fatal("without a recovery the latest usage must be returned as is")
	}
}

func TestRetiredTurnsCopyIsSafeAgainstSwitchRootThread(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
	snap := &recoverySnapshot{oldThreadID: oldThread, oldTurnID: oldTurn}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			if copied := f.s.retiredSnapshotPlus(); copied == nil {
				t.Error("no copy")
			}
			_ = f.s.isRetired(oldTurn)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 200 {
			f.s.steering.mu.Lock()
			f.s.steering.id = "turn-" + itoa(i)
			f.s.steering.mu.Unlock()
			_ = f.s.switchRootThread("thread-"+itoa(i), snap)
		}
	}()
	wg.Wait()
	if !f.s.isRetired(oldTurn) {
		t.Fatal("the replaced Turn was not retired")
	}
}

func TestRecoveryStaleTurnIsNotRestarted(t *testing.T) {
	// A successor Session ignores every Turn its predecessor owned, including the
	// failed one a recovery replaced.
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
	f.fail(msgReasoning, usageFailed)
	f.eventually("the new Turn to be acknowledged", f.acked)
	retired := f.s.retiredSnapshotPlus()
	if !retired[oldTurn] || !retired[newTurn] {
		t.Fatalf("retired = %v", retired)
	}
}

// Dropping the images the user just sent would change what they asked, so the
// failure is final; this is the one abandonment decided only after history is read.
func TestCurrentInputImagesAreNeverDropped(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassImageInput), history: fixtureHistory("history-image"), input: imageInput()})
	f.fail(msgNoImage, usageFailed)
	result := summarize(t, f.frames())
	if f.native.count("thread/start") != 0 || result.dones != 1 || len(result.errors) != 1 {
		t.Fatalf("thread/start=%d terminal=%+v", f.native.count("thread/start"), result)
	}
	if result.errors[0].Fingerprint != proto.FingerprintModelNoImageInput || result.errors[0].Code != "model_input_unsupported_image" {
		t.Fatalf("the classified failure was lost: %+v", result.errors[0])
	}
	if len(result.recovery) != 1 || result.recovery[0].Reason != proto.RecoveryAbortCurrentInputHasImages || result.done.Usage.InputTokens != 100 {
		t.Fatalf("records = %+v usage = %+v", result.recovery, result.done.Usage)
	}
}

// turn/failed carries no error to classify; it keeps its existing, unrecovered path.
func TestTurnFailedNotificationIsNotRecovered(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
	f.s.onTurnFailed(json.RawMessage(`{"threadId":"` + oldThread + `","turn":{"id":"` + oldTurn + `","status":"failed","error":{"message":"` + strings.ReplaceAll(msgReasoning, `"`, `\"`) + `"}}}`))
	result := summarize(t, f.frames())
	if f.native.count("thread/start") != 0 || f.reader.calls.Load() != 0 || result.dones != 1 || len(result.errors) != 1 || result.errors[0].Fingerprint != "" {
		t.Fatalf("thread/start=%d terminal=%+v", f.native.count("thread/start"), result)
	}
}

// Ending the Turn ends the recovery's right to hold the Turn's steering.
func TestStopSteeringClearsRecovering(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning")})
	f.native.hold["thread/start"] = nil
	f.fail(msgReasoning, usageFailed)
	f.eventually("thread/start", f.seen("thread/start"))
	f.s.steering.mu.Lock()
	recovering := f.s.steering.recovering
	f.s.steering.mu.Unlock()
	if !recovering {
		t.Fatal("not marked recovering")
	}
	turnID, active, deferred := f.s.stopSteeringForCancel()
	if turnID != oldTurn || !active || !deferred {
		t.Fatalf("stop = %q %t %t: the failed Turn is terminal, so cancellation defers to the recovery", turnID, active, deferred)
	}
	if err := f.s.Steer(t.Context(), proto.PromptSteerPayload{InputID: "i", Input: proto.TextInput("x")}); !errors.Is(err, agent.ErrSteeringInactive) {
		t.Fatalf("after stop a steer must answer inactive, got %v", err)
	}
	f.s.steering.mu.Lock()
	cleared := !f.s.steering.recovering && f.s.steering.recoveryCancel == nil
	f.s.steering.mu.Unlock()
	if !cleared {
		t.Fatal("recovering state survived stopSteering")
	}
	summarize(t, f.frames())
}

// Every abandonment goes through abandonRecovery with one of the proto reasons;
// each has a test that it ends exactly as the run without recovery, so a new
// reason cannot be added without one.
func TestEveryAbortReasonIsExercised(t *testing.T) {
	exercised := map[string]string{
		proto.RecoveryAbortNotApplicable:         "TestAbandonedRecoveryMatchesTheRunWithoutRecovery/history has nothing to drop",
		proto.RecoveryAbortCurrentInputHasImages: "TestCurrentInputImagesAreNeverDropped",
		proto.RecoveryAbortHistoryUnreadable:     "TestAbandonedRecoveryMatchesTheRunWithoutRecovery/history unreadable",
		proto.RecoveryAbortThreadStartFailed:     "TestAbandonedRecoveryMatchesTheRunWithoutRecovery/thread/start refused",
		proto.RecoveryAbortTurnStartFailed:       "TestAbandonedRecoveryMatchesTheRunWithoutRecovery/turn/start refused",
		proto.RecoveryAbortCancelled:             "TestRecoveryCancellationWins",
		proto.RecoveryAbortModelChanged:          "TestAbandonedRecoveryMatchesTheRunWithoutRecovery/thread/start resolves another model",
	}
	for _, reason := range proto.RecoveryAbortReasons() {
		if exercised[reason] == "" {
			t.Fatalf("abort reason %q has no test", reason)
		}
	}
	if len(exercised) != len(proto.RecoveryAbortReasons()) {
		t.Fatal("a test claims a reason the protocol does not define")
	}
}
func TestRecoveryDefersRebuiltTerminalUntilAcknowledged(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
	f.native.before["turn/start"] = func(JsonRpcRequest) {
		// The rebuilt Turn starts and completes before the RPC reply.
		f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
		f.s.appendFinalText("done")
		f.native.notify("turn/completed", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`","status":"completed"}}`))
	}
	f.fail(msgReasoning, usageFailed)
	f.eventually("the new Turn to be acknowledged", f.acked)
	result := summarize(t, f.frames())
	if result.dones != 1 || len(result.errors) != 0 {
		t.Fatalf("terminal = %+v", result)
	}
	if len(result.recovery) != 1 || result.recovery[0].Outcome != proto.RecoveryOutcomeRecovered || result.recovery[0].NewNativeSessionID != newThread {
		t.Fatalf("recovery records = %+v", result.recovery)
	}
	if got := result.done.Metadata[proto.DoneMetaAgentSessionID]; got != newThread {
		t.Fatalf("native session id = %v, want %s", got, newThread)
	}
}
func TestRecoveryDefersRebuiltFailureUntilAcknowledged(t *testing.T) {
	f := newRecoveryFixture(t, fixtureOptions{controls: enabledControls(proto.RecoveryClassReasoningState), history: fixtureHistory("reasoning"), settle: true})
	f.native.before["turn/start"] = func(JsonRpcRequest) {
		// The rebuilt Turn starts and fails before the RPC reply.
		f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
		f.native.notify("turn/failed", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`","status":"failed"}}`))
	}
	f.fail(msgReasoning, usageFailed)
	f.eventually("the new Turn to be acknowledged", f.acked)
	result := summarize(t, f.frames())
	if len(result.errors) != 1 {
		t.Fatalf("terminal = %+v", result)
	}
	if len(result.recovery) != 1 || result.recovery[0].Outcome != proto.RecoveryOutcomeExhausted || result.recovery[0].NewNativeSessionID != newThread {
		t.Fatalf("recovery records = %+v: the failure must not be reported before the recovery commits", result.recovery)
	}
}
