package codex

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/agent"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

func gateFixture(t *testing.T, o fixtureOptions) *recoveryFixture {
	t.Helper()
	o.controls = enabledControls(proto.RecoveryClassReasoningState)
	o.history = fixtureHistory("reasoning")
	f := newRecoveryFixture(t, o)
	f.s.functions, _ = prepareFunctionTools([]proto.FunctionTool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}})
	return f
}

func (f *recoveryFixture) toolCall(callID string) {
	f.native.write(map[string]any{"jsonrpc": "2.0", "id": "srv-" + callID, "method": "item/tool/call", "params": map[string]any{
		"threadId": newThread, "turnId": newTurn, "callId": callID, "tool": "lookup", "arguments": map[string]any{},
	}})
}

func (f *recoveryFixture) startRebuilt() {
	f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
	f.native.notify("item/agentMessage/delta", json.RawMessage(`{"threadId":"`+newThread+`","turnId":"`+newTurn+`","itemId":"m1","delta":"hello"}`))
}

func (f *recoveryFixture) gateQueued(n int) func() bool {
	return func() bool {
		f.s.steering.mu.Lock()
		defer f.s.steering.mu.Unlock()
		return f.s.steering.gate != nil && len(f.s.steering.gate.queue) >= n
	}
}

func (f *recoveryFixture) gateWaiters(n int) func() bool {
	return func() bool {
		f.s.steering.mu.Lock()
		defer f.s.steering.mu.Unlock()
		return f.s.steering.gate != nil && f.s.steering.gate.waiters >= n
	}
}

func (f *recoveryFixture) gateGone() bool {
	f.s.steering.mu.Lock()
	defer f.s.steering.mu.Unlock()
	return f.s.steering.gate == nil
}

// drain returns what the Session has emitted so far without waiting.
func (f *recoveryFixture) drain() []proto.Envelope {
	var out []proto.Envelope
	for {
		select {
		case env, ok := <-f.out:
			if !ok {
				return out
			}
			out = append(out, env)
		default:
			return out
		}
	}
}

func countType(frames []proto.Envelope, typ string) int {
	n := 0
	for _, env := range frames {
		if env.Type == typ {
			n++
		}
	}
	return n
}

func (f *recoveryFixture) pendingCalls() int {
	f.s.functions.mu.Lock()
	defer f.s.functions.mu.Unlock()
	return len(f.s.functions.pending)
}

func TestRecoveryHoldsRebuiltTrafficUntilAcknowledged(t *testing.T) {
	f := gateFixture(t, fixtureOptions{settle: true})
	release := make(chan struct{})
	f.native.hold["turn/start"] = release
	f.fail(msgReasoning, usageFailed)
	f.eventually("turn/start", f.seen("turn/start"))
	f.startRebuilt()
	f.toolCall("c1")
	f.eventually("the traffic to be held", f.gateQueued(2))
	f.eventually("the function call to wait", f.gateWaiters(1))
	time.Sleep(50 * time.Millisecond)
	before := f.drain()
	if countType(before, proto.TypeDelta) != 0 || countType(before, proto.TypeFunctionCall) != 0 || f.pendingCalls() != 0 {
		t.Fatalf("rebuilt traffic escaped before the acknowledgement: %+v pending=%d", before, f.pendingCalls())
	}
	close(release)
	f.eventually("the new Turn to be acknowledged", f.acked)
	f.eventually("the function call to be published", func() bool { return f.pendingCalls() == 1 })
	f.finishNewTurn("done")
	result := summarize(t, append(before, f.frames()...))
	if countType(result.raw, proto.TypeDelta) != 1 || countType(result.raw, proto.TypeFunctionCall) != 1 || result.dones != 1 {
		t.Fatalf("held traffic was not replayed exactly once: %+v", result)
	}
}

func TestRecoveryDropsHeldTrafficWhenTurnStartFails(t *testing.T) {
	f := gateFixture(t, fixtureOptions{settle: true})
	release := make(chan struct{})
	f.native.hold["turn/start"] = release
	f.native.fail["turn/start"] = "rejected"
	f.fail(msgReasoning, usageFailed)
	f.eventually("turn/start", f.seen("turn/start"))
	f.startRebuilt()
	f.toolCall("c1")
	f.eventually("the traffic to be held", f.gateQueued(2))
	f.eventually("the function call to wait", f.gateWaiters(1))
	close(release)
	result := summarize(t, f.frames())
	if countType(result.raw, proto.TypeDelta) != 0 || countType(result.raw, proto.TypeFunctionCall) != 0 || f.pendingCalls() != 0 {
		t.Fatalf("a failed turn/start must not have published or run anything: %+v", result.raw)
	}
	if result.dones != 1 || len(result.errors) != 1 || !f.gateGone() || !f.s.recovery.unconfirmed.Load() {
		t.Fatalf("dones=%d errors=%d gateGone=%v unconfirmed=%v", result.dones, len(result.errors), f.gateGone(), f.s.recovery.unconfirmed.Load())
	}
}

func TestRecoveryDropsHeldTrafficWhenCancelledBeforeAcknowledgement(t *testing.T) {
	f := gateFixture(t, fixtureOptions{})
	f.native.hold["turn/start"] = nil
	f.fail(msgReasoning, usageFailed)
	f.eventually("turn/start", f.seen("turn/start"))
	f.startRebuilt()
	f.toolCall("c1")
	f.eventually("the traffic to be held", f.gateQueued(2))
	f.s.cancelFn()
	f.eventually("the gate to be dropped", f.gateGone)
	time.Sleep(50 * time.Millisecond)
	frames := f.drain()
	if countType(frames, proto.TypeDelta) != 0 || countType(frames, proto.TypeFunctionCall) != 0 || f.pendingCalls() != 0 {
		t.Fatalf("cancelled recovery published held traffic: %+v", frames)
	}
}

func TestRecoveryAbandonsWhenHeldTrafficOverflows(t *testing.T) {
	f := gateFixture(t, fixtureOptions{settle: true})
	release := make(chan struct{})
	f.native.hold["turn/start"] = release
	f.fail(msgReasoning, usageFailed)
	f.eventually("turn/start", f.seen("turn/start"))
	for i := 0; i <= maxGatedFrames; i++ {
		f.native.notify("item/agentMessage/delta", json.RawMessage(`{"threadId":"`+newThread+`","turnId":"`+newTurn+`","itemId":"m1","delta":"x"}`))
	}
	f.eventually("the recovery to be abandoned", f.gateGone)
	close(release)
	result := summarize(t, f.frames())
	if countType(result.raw, proto.TypeDelta) != 0 || result.dones != 1 {
		t.Fatalf("overflow must abandon the recovery without publishing: dones=%d", result.dones)
	}
	if !f.s.recovery.unconfirmed.Load() || len(result.recovery) != 1 || result.recovery[0].Outcome != proto.RecoveryOutcomeAborted || result.recovery[0].Reason != proto.RecoveryAbortTurnStartFailed {
		t.Fatalf("recovery records = %+v unconfirmed=%v", result.recovery, f.s.recovery.unconfirmed.Load())
	}
}

func TestRecoveryKeepsSteeringGatedWhileReplaying(t *testing.T) {
	f := gateFixture(t, fixtureOptions{settle: true})
	f.s.steering.mu.Lock()
	f.s.steering.replaying = true
	f.s.steering.mu.Unlock()
	err := f.s.Steer(t.Context(), proto.PromptSteerPayload{Input: proto.TextInput("more")})
	if !errors.Is(err, agent.ErrSteeringNotReady) {
		t.Fatalf("steer during replay = %v, want ErrSteeringNotReady", err)
	}
	if f.native.count("turn/steer") != 0 {
		t.Fatal("input was written during the replay")
	}
}

func TestRecoveryReplaysDeferredTerminalBeforeReopeningSteering(t *testing.T) {
	f := gateFixture(t, fixtureOptions{settle: true})
	f.native.before["turn/start"] = func(JsonRpcRequest) {
		f.native.notify("turn/started", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`"}}`))
		f.native.notify("turn/completed", json.RawMessage(`{"threadId":"`+newThread+`","turn":{"id":"`+newTurn+`","status":"completed"}}`))
	}
	f.fail(msgReasoning, usageFailed)
	result := summarize(t, f.frames())
	if result.dones != 1 || len(result.errors) != 0 {
		t.Fatalf("terminal = %+v", result)
	}
	f.s.steering.mu.Lock()
	replaying, gate := f.s.steering.replaying, f.s.steering.gate
	f.s.steering.mu.Unlock()
	if replaying || gate != nil {
		t.Fatalf("replaying=%v gate=%v after the terminal settled", replaying, gate)
	}
	if err := f.s.Steer(t.Context(), proto.PromptSteerPayload{Input: proto.TextInput("late")}); !errors.Is(err, agent.ErrSteeringInactive) {
		t.Fatalf("steer after the terminal = %v, want ErrSteeringInactive", err)
	}
}
