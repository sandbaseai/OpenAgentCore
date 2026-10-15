package codex

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Upstream recovery: when the model provider rejects a Turn with a recognized,
// recoverable HTTP 400 (design 4.2), the failed Turn is not settled. A fresh
// thread is built from a rendered transcript of the old one and the same Turn
// continues there. The Turn's terminal result is still emitted exactly once.

const (
	recoveryIdle int32 = iota
	recoveryRecovering
)

// recoveryTimeouts bound every blocking step of one recovery. Zero fields take
// the defaults; tests shorten them.
type recoveryTimeouts struct {
	Total, History, ThreadStart, TurnStart, Interrupt time.Duration
}

func (t recoveryTimeouts) resolved() recoveryTimeouts {
	for _, field := range []struct {
		value *time.Duration
		def   time.Duration
	}{{&t.Total, 40 * time.Second}, {&t.History, 10 * time.Second}, {&t.ThreadStart, 15 * time.Second}, {&t.TurnStart, 15 * time.Second}, {&t.Interrupt, 2 * time.Second}} {
		if *field.value <= 0 {
			*field.value = field.def
		}
	}
	return t
}

// sessionRecovery is one Turn's recovery state.
type sessionRecovery struct {
	state atomic.Int32
	// unconfirmed marks a native Turn whose start may have taken effect without
	// being acknowledged; its process is never reused.
	unconfirmed atomic.Bool

	// Fixed before the Session is published.
	controls proto.UpstreamRecoveryControls
	plan     *SessionPlan
	input    proto.MessageInput
	reader   historyReader
	timeouts recoveryTimeouts

	// gateThread is the thread whose late traffic is dropped while recovering.
	// Guarded by Session.threadIDMu.
	gateThread string
	snapshot   atomic.Pointer[recoverySnapshot]

	mu       sync.Mutex
	attempts int
	used     map[string]bool
	records  []proto.UpstreamRecovery
}

// recoverySnapshot is what the failed Turn left behind at the moment a recovery
// took it over. A recovery that does not end in an acknowledged new Turn
// restores it, so the Turn then ends exactly as it would without recovery.
type recoverySnapshot struct {
	p               TurnCompletedNotification
	oldThreadID     string
	oldTurnID       string
	savedGen        uint64
	finalText       string
	lastErrText     string
	latestUsage     *TurnUsage
	usageTotal      TurnUsage
	usageBaseline   TurnUsage
	usageTurnID     string
	usageCarry      *TurnUsage
	usageInRecovery bool
	usageSwitched   bool
	resolvedModel   string
	fingerprint     string
	// recorded is guarded by recovery.mu: the attempt already left its record.
	recorded bool
}

// recoveryRun is the recovery goroutine's view of one attempt.
type recoveryRun struct {
	fingerprint string
	snap        *recoverySnapshot
	dropped     proto.RecoveryDropped

	mu          sync.Mutex
	newThreadID string
	newModel    string
	newTurnID   string
	// ambiguous: the new Turn may have started natively without an acknowledgement.
	ambiguous bool
	acked     bool
}

func (r *recoveryRun) set(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *recoveryRun) get() (newThreadID, newTurnID string, ambiguous, acked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.newThreadID, r.newTurnID, r.ambiguous, r.acked
}

var errRecoverySwitchRefused = errors.New("codex: recovery switch refused")

// recoveryGateClosed reports whether a notification for threadID belongs to the
// failed Turn's thread while its recovery is under way. It is shared by every
// handler that would otherwise record that traffic.
func (s *Session) recoveryGateClosed(threadID string) bool {
	if s.recovery.state.Load() != recoveryRecovering {
		return false
	}
	s.threadIDMu.Lock()
	defer s.threadIDMu.Unlock()
	return threadID != "" && threadID == s.recovery.gateThread
}

// recoverySnapshotFinalText is the failed Turn's streamed text while a recovery
// holds it, empty otherwise.
func (s *Session) recoverySnapshotFinalText() string {
	if snap := s.recovery.snapshot.Load(); snap != nil {
		return snap.finalText
	}
	return ""
}

func (s *Session) historyReader() historyReader {
	if s.recovery.reader != nil {
		return s.recovery.reader
	}
	return rpcHistoryReader{rpc: s.rpc}
}

// shouldRecover decides, without any RPC, whether the failed root Turn p is
// recoverable. The caller holds notificationMu.
func (s *Session) shouldRecover(p TurnCompletedNotification) (fingerprint string, ok bool) {
	c := s.recovery.controls
	if !c.Enabled || s.executor == nil || s.subagents != nil || s.recovery.plan == nil {
		return "", false
	}
	if strings.ToLower(p.Turn.Status) != "failed" {
		return "", false
	}
	fingerprint = classifyTurnError(p.Turn.Error).Fingerprint
	allowed := false
	for _, class := range c.Classes {
		if want, known := proto.RecoveryClassFingerprint(class); known && want == fingerprint {
			allowed = true
		}
	}
	if !allowed {
		return "", false
	}
	if s.recovery.state.Load() != recoveryIdle || s.cancelled.Load() || s.cancelCtx.Err() != nil || s.terminal.Load() {
		return "", false
	}
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	if s.recovery.attempts >= c.MaxAttempts || s.recovery.used[fingerprint] {
		return "", false
	}
	return fingerprint, true
}

// beginRecovery takes over the failed Turn p and starts the recovery goroutine.
// It runs under notificationMu and performs no RPC. It reports false, leaving
// everything untouched, when the Turn must settle normally instead.
func (s *Session) beginRecovery(p TurnCompletedNotification, fingerprint string) bool {
	if !s.beginOperation() {
		return false
	}
	if !s.recovery.state.CompareAndSwap(recoveryIdle, recoveryRecovering) {
		s.endOperation()
		return false
	}
	timeouts := s.recovery.timeouts.resolved()
	ctx, cancel := context.WithTimeout(s.cancelCtx, timeouts.Total)

	// Close the in-flight steer, if any, in the same critical section that marks
	// the Turn as being recovered. The slot is only cancelled, not stopped: stopping
	// is reserved for cancellation and for the end of the Turn.
	s.steering.mu.Lock()
	if s.steering.stopped {
		s.steering.mu.Unlock()
		s.recovery.state.Store(recoveryIdle)
		cancel()
		s.endOperation()
		return false
	}
	savedCancel, savedGen := s.steering.cancel, s.steering.cancelGen
	oldTurnID := s.steering.id
	s.steering.recovering, s.steering.recoveringTurnID = true, oldTurnID
	s.steering.recoveryCancel = cancel
	if savedCancel != nil {
		savedCancel()
	}
	s.steering.mu.Unlock()

	// Snapshot what the failed Turn buffered, then move its usage into the carry.
	// The failed Turn's usage is published later, once, as part of the total.
	s.threadIDMu.Lock()
	oldThread := s.threadID
	if s.reportedID == "" {
		s.reportedID = oldThread
	}
	s.recovery.gateThread = oldThread
	s.usageMu.Lock()
	s.finalTextMu.Lock()
	snap := &recoverySnapshot{
		p: p, oldThreadID: oldThread, oldTurnID: oldTurnID, savedGen: savedGen,
		finalText: s.finalText, lastErrText: s.lastErrText,
		latestUsage: s.latestUsage, usageTotal: s.usageTotal, usageBaseline: s.usageBaseline, usageTurnID: s.usageTurnID,
		usageCarry: s.usageCarry, usageInRecovery: s.usageInRecovery, usageSwitched: s.usageSwitched,
		resolvedModel: s.model(), fingerprint: fingerprint,
	}
	failed := p.Turn.Usage
	if failed == nil {
		failed = s.latestUsage
	}
	s.usageCarry = addUsage(s.usageCarry, failed)
	s.usageInRecovery, s.usageSwitched = true, false
	s.usageThreadID = oldThread
	s.finalTextMu.Unlock()
	s.usageMu.Unlock()
	s.threadIDMu.Unlock()
	s.recovery.snapshot.Store(snap)

	s.recovery.mu.Lock()
	attempt := s.recovery.attempts + 1
	s.recovery.mu.Unlock()
	s.cfg.logger.Warn("codex: upstream recovery started",
		"run_id", s.runID, "fingerprint", fingerprint, "attempt", attempt)
	run := &recoveryRun{fingerprint: fingerprint, snap: snap}
	go s.runRecovery(ctx, cancel, run)
	return true
}

// ownerStopped reports that the Turn's owner asked to stop or already ended it.
func (s *Session) ownerStopped() bool {
	if s.cancelled.Load() || s.cancelCtx.Err() != nil {
		return true
	}
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	return s.steering.stopped
}

// stepReason is the abort reason for a failed step: cancellation outranks the
// step's own reason.
func (s *Session) stepReason(reason string) string {
	if s.ownerStopped() {
		return proto.RecoveryAbortCancelled
	}
	return reason
}

// recoveryThreadUsed reports whether a thread already served this Turn as a
// recovery source or target, so a rebuild never lands in an earlier thread.
func (s *Session) recoveryThreadUsed(id string) bool {
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	for _, r := range s.recovery.records {
		if r.OldNativeSessionID == id || r.NewNativeSessionID == id {
			return true
		}
	}
	return false
}

func (s *Session) runRecovery(ctx context.Context, cancel context.CancelFunc, run *recoveryRun) {
	defer s.endOperation()
	defer cancel()
	t := s.recovery.timeouts.resolved()
	snap := run.snap

	historyCtx, stop := context.WithTimeout(ctx, t.History)
	turns, err := s.historyReader().ReadHistory(historyCtx, snap.oldThreadID)
	stop()
	if s.terminal.Load() {
		s.endRecovery()
		return
	}
	if err != nil {
		s.abandonRecovery(run, s.stepReason(proto.RecoveryAbortHistoryUnreadable))
		return
	}

	input, reason := planRecovery(run.fingerprint, turns, snap.oldTurnID, s.recovery.input, s.recovery.controls.TranscriptMaxChars)
	if reason != "" {
		s.abandonRecovery(run, s.stepReason(reason))
		return
	}
	run.set(func() { run.dropped = input.dropped })

	startParams := s.buildThreadStartParams(*s.recovery.plan)
	startCtx, stop := context.WithTimeout(ctx, t.ThreadStart)
	_, err = s.rpc.requestWithTimeout(startCtx, "thread/start", startParams, func(frame any) error { return s.rpc.writeFrameContext(startCtx, frame) }, 0, func(raw json.RawMessage) error {
		var res ThreadStartResult
		if err := json.Unmarshal(raw, &res); err != nil || res.Thread.ID == "" || res.Thread.ID == snap.oldThreadID || s.recoveryThreadUsed(res.Thread.ID) {
			return errors.New("codex: recovery thread response has no new identity")
		}
		run.set(func() { run.newThreadID, run.newModel = res.Thread.ID, res.Model })
		return nil
	})
	stop()
	newThreadID, _, _, _ := run.get()
	if s.terminal.Load() {
		s.endRecovery()
		return
	}
	if err != nil || newThreadID == "" {
		s.abandonRecovery(run, s.stepReason(proto.RecoveryAbortThreadStartFailed))
		return
	}

	// Tokens already carried belong to the old model; a rebuilt thread that
	// resolves another model would mislabel them, so abandon instead.
	run.mu.Lock()
	newModel := run.newModel
	run.mu.Unlock()
	if newModel != "" && newModel != snap.resolvedModel {
		s.abandonRecovery(run, s.stepReason(proto.RecoveryAbortModelChanged))
		return
	}
	mode, err := s.collaborationMode(*s.recovery.plan)
	if err != nil {
		s.abandonRecovery(run, proto.RecoveryAbortTurnStartFailed)
		return
	}
	if err := s.switchRootThread(newThreadID, snap); err != nil {
		if s.terminal.Load() {
			s.endRecovery()
			return
		}
		s.abandonRecovery(run, proto.RecoveryAbortCancelled)
		return
	}

	params := TurnStartParams{ThreadID: newThreadID, Input: input.native, CollaborationMode: mode}
	turnCtx, stop := context.WithTimeout(ctx, t.TurnStart)
	_, err = s.rpc.requestWithTimeout(turnCtx, "turn/start", params, func(frame any) error { return s.rpc.writeFrameContext(turnCtx, frame) }, 0, func(raw json.RawMessage) error {
		return s.acknowledgeRecovery(raw, run)
	})
	stop()
	_, newTurnID, _, acked := run.get()
	if !acked {
		if s.terminal.Load() {
			s.endRecovery()
			return
		}
		var rejected *JsonRpcError
		// Only an explicit refusal proves the new Turn never started, and only if
		// no turn/started for the rebuilt thread was observed before that reply.
		s.steering.mu.Lock()
		startObserved := s.steering.id != "" || (s.steering.gate != nil && s.steering.gate.saw)
		s.steering.mu.Unlock()
		if !errors.As(err, &rejected) || startObserved {
			run.set(func() { run.ambiguous = true })
		}
		s.abandonRecovery(run, s.stepReason(proto.RecoveryAbortTurnStartFailed))
		return
	}
	// The new Turn is native and owned by the ordinary machinery from here on.
	// An owner that asked to stop meanwhile has not interrupted it yet.
	if !s.terminal.Load() && s.ownerStopped() {
		s.interruptBestEffort(newThreadID, newTurnID, t.Interrupt)
	}
}

// switchRootThread routes the Session to the rebuilt thread. It is refused once
// the owner stopped the Turn or the Turn ended, so cancellation always wins.
func (s *Session) switchRootThread(newThreadID string, snap *recoverySnapshot) error {
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	if s.steering.stopped || s.cancelCtx.Err() != nil || s.terminal.Load() {
		return errRecoverySwitchRefused
	}
	s.threadIDMu.Lock()
	defer s.threadIDMu.Unlock()
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.finalTextMu.Lock()
	defer s.finalTextMu.Unlock()
	s.threadID = newThreadID
	if s.retiredTurns == nil {
		s.retiredTurns = map[string]bool{}
	}
	s.retiredTurns[snap.oldTurnID] = true
	s.steering.id = ""
	s.steering.gate = newRecoveryGate(newThreadID)
	// Clear the cancel slot only if it still holds the steer closed at entry.
	if s.steering.cancelGen == snap.savedGen {
		s.steering.cancel = nil
	}
	s.usageTotal, s.usageBaseline, s.usageTurnID, s.latestUsage = TurnUsage{}, TurnUsage{}, "", nil
	s.usageSwitched = true
	s.usageThreadID = newThreadID
	// Text the failed Turn already streamed stays: Done carries the full content.
	s.lastErrText = ""
	return nil
}

// acknowledgeRecovery runs on the RPC reader with the turn/start reply, before
// any later notification: the new Turn is bound and the recovery's bookkeeping
// moves from "attempt in progress" to "attempt completed".
func (s *Session) acknowledgeRecovery(raw json.RawMessage, run *recoveryRun) error {
	if err := s.bindTurnResult(raw); err != nil {
		return err
	}
	var res struct {
		Turn Turn `json:"turn"`
	}
	_ = json.Unmarshal(raw, &res)
	// A cancellation can win the race against this reply: bindTurnResult then
	// leaves the new Turn unbound. Treat that as ambiguous instead of acknowledged.
	s.steering.mu.Lock()
	bound := s.steering.id == res.Turn.ID
	s.steering.mu.Unlock()
	if !bound {
		return errors.New("codex: recovery turn was not bound to the session")
	}
	s.steering.mu.Lock()
	// Held traffic that outgrew its buffer was already dropped; the recovery is
	// being abandoned, so the reply must not commit it. Checked under the lock
	// that detaches the gate, so no later overflow can slip past.
	if s.steering.gate != nil && s.steering.gate.overflow {
		s.steering.mu.Unlock()
		return errRecoveryGateOverflow
	}
	run.set(func() { run.newTurnID, run.acked = res.Turn.ID, true })
	newThreadID, _, _, _ := run.get()
	s.steering.recovering, s.steering.recoveringTurnID = false, ""
	s.steering.recoveryCancel = nil
	// Input stays refused until held traffic and deferred terminals are settled.
	s.steering.replaying = true
	gate := s.steering.gate
	var held []gatedFrame
	if gate != nil {
		held, gate.queue, gate.bytes = gate.queue, nil, 0
		s.steering.gate = nil
	}
	deferred := s.steering.deferredCompleted
	deferredFailed := s.steering.deferredFailed
	s.steering.deferredCompleted, s.steering.deferredFailed = nil, nil
	s.steering.mu.Unlock()

	s.threadIDMu.Lock()
	s.reportedID = newThreadID
	s.recovery.gateThread = ""
	s.usageMu.Lock()
	s.usageInRecovery, s.usageSwitched = false, false
	s.usageMu.Unlock()
	s.threadIDMu.Unlock()

	run.mu.Lock()
	dropped := run.dropped
	run.mu.Unlock()
	s.recovery.mu.Lock()
	for i := range s.recovery.records {
		// A recovery followed by another one was itself exhausted.
		if s.recovery.records[i].Attempt >= 1 {
			s.recovery.records[i].Outcome = proto.RecoveryOutcomeExhausted
		}
	}
	s.recovery.attempts++
	if s.recovery.used == nil {
		s.recovery.used = map[string]bool{}
	}
	s.recovery.used[run.fingerprint] = true
	run.snap.recorded = true
	s.appendRecoveryLocked(proto.UpstreamRecovery{
		Fingerprint: run.fingerprint, Action: proto.RecoveryActionRebuildThread, Attempt: s.recovery.attempts,
		Outcome: proto.RecoveryOutcomeRecovered, Dropped: dropped,
		OldNativeSessionID: run.snap.oldThreadID, NewNativeSessionID: newThreadID,
	})
	s.recovery.mu.Unlock()
	s.recovery.snapshot.Store(nil)
	s.recovery.state.Store(recoveryIdle)
	s.cfg.logger.Info("codex: upstream recovery switched thread",
		"run_id", s.runID, "fingerprint", run.fingerprint, "attempt", s.recovery.attemptCount())
	s.replayGated(held)
	if deferred != nil {
		s.onTurnCompleted(deferred)
	}
	if deferredFailed != nil {
		s.onTurnFailed(deferredFailed)
	}
	s.steering.mu.Lock()
	s.steering.replaying = false
	if gate != nil {
		gate.finishLocked(false)
	}
	s.steering.mu.Unlock()
	return nil
}

func (r *sessionRecovery) attemptCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// appendRecoveryLocked records an entry while the bounded list has room.
func (s *Session) appendRecoveryLocked(record proto.UpstreamRecovery) {
	if len(s.recovery.records) < proto.MaxUpstreamRecoveries {
		s.recovery.records = append(s.recovery.records, record)
	}
}

// restoreFromSnapshot returns the Session to the failed Turn's state: routing,
// the Turn id, usage and the buffered text. It is idempotent for a recovery that
// never switched, so every abandonment can call it.
func (s *Session) restoreFromSnapshot(snap *recoverySnapshot) {
	s.steering.mu.Lock()
	defer s.steering.mu.Unlock()
	s.threadIDMu.Lock()
	defer s.threadIDMu.Unlock()
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.finalTextMu.Lock()
	defer s.finalTextMu.Unlock()
	s.threadID = snap.oldThreadID
	// A model resolved by an abandoned recovery thread must not outlive it.
	s.setModel(snap.resolvedModel)
	s.steering.id = snap.oldTurnID
	s.latestUsage, s.usageTotal, s.usageBaseline, s.usageTurnID = snap.latestUsage, snap.usageTotal, snap.usageBaseline, snap.usageTurnID
	s.usageCarry, s.usageInRecovery, s.usageSwitched = snap.usageCarry, snap.usageInRecovery, snap.usageSwitched
	// Routing is old again, so the usage state belongs to the old thread.
	s.usageThreadID = snap.oldThreadID
	s.finalText, s.lastErrText = snap.finalText, snap.lastErrText
	s.steering.deferredCompleted, s.steering.deferredFailed = nil, nil
	s.dropGateLocked()
}

// abandonRecovery ends a recovery that did not produce an acknowledged new Turn.
// Every abandonment restores the snapshot first and then settles the original
// failure through the same path as without recovery. reason is one of
// proto.RecoveryAbort*.
func (s *Session) abandonRecovery(run *recoveryRun, reason string) {
	snap := run.snap
	if s.terminal.Load() {
		s.endRecovery()
		return
	}
	s.restoreFromSnapshot(snap)
	_, _, ambiguous, _ := run.get()
	if ambiguous {
		s.recovery.unconfirmed.Store(true)
	}
	s.recovery.mu.Lock()
	snap.recorded = true
	s.appendRecoveryLocked(proto.UpstreamRecovery{
		Fingerprint: run.fingerprint, Action: proto.RecoveryActionRebuildThread, Attempt: 0,
		Outcome: proto.RecoveryOutcomeAborted, Reason: reason, OldNativeSessionID: snap.oldThreadID,
	})
	s.recovery.mu.Unlock()
	s.cfg.logger.Warn("codex: upstream recovery abandoned",
		"run_id", s.runID, "fingerprint", run.fingerprint, "reason", reason)
	s.outcome.notificationMu.Lock()
	if !s.terminal.Load() {
		s.completeTurnLocked(snap.p)
	}
	s.outcome.notificationMu.Unlock()
	s.endRecovery()
}

// endRecovery leaves the recovering state. Whoever ended the Turn owns its
// terminal; this only releases the recovery's own bookkeeping.
func (s *Session) endRecovery() {
	s.steering.mu.Lock()
	s.steering.recovering, s.steering.recoveringTurnID = false, ""
	s.steering.recoveryCancel = nil
	s.steering.deferredCompleted, s.steering.deferredFailed = nil, nil
	s.dropGateLocked()
	s.steering.mu.Unlock()
	s.threadIDMu.Lock()
	s.recovery.gateThread = ""
	s.threadIDMu.Unlock()
	s.recovery.snapshot.Store(nil)
	s.recovery.state.Store(recoveryIdle)
}

// interruptBestEffort asks native to stop a Turn that no owner is waiting on.
func (s *Session) interruptBestEffort(threadID, turnID string, timeout time.Duration) {
	if threadID == "" || turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _ = s.rpc.request(ctx, "turn/interrupt", TurnInterruptParams{ThreadID: threadID, TurnID: turnID}, func(frame any) error {
		return s.rpc.writeFrameContext(ctx, frame)
	})
}

// recordInflightRecoveryAbort runs once a terminal won the race against an
// in-flight recovery (process or stream ended mid-recovery). It records the
// attempt before Done is emitted; the recovery goroutine then sees the terminal
// and leaves it alone.
func (s *Session) recordInflightRecoveryAbort() {
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	snap := s.recovery.snapshot.Load()
	if snap == nil || snap.recorded {
		return
	}
	snap.recorded = true
	s.appendRecoveryLocked(proto.UpstreamRecovery{
		Fingerprint: snap.fingerprint, Action: proto.RecoveryActionRebuildThread, Attempt: 0,
		Outcome: proto.RecoveryOutcomeAborted, Reason: proto.RecoveryAbortCancelled, OldNativeSessionID: snap.oldThreadID,
	})
}

// attachRecoveries adds the Turn's recovery records to Done metadata. failed
// marks the last completed attempt exhausted: its new Turn did not complete.
func (s *Session) attachRecoveries(meta map[string]any, failed bool) {
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	if len(s.recovery.records) == 0 {
		return
	}
	records := slices.Clone(s.recovery.records)
	if failed {
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].Attempt >= 1 {
				records[i].Outcome = proto.RecoveryOutcomeExhausted
				break
			}
		}
	}
	meta[proto.DoneMetaUpstreamRecoveries] = records
}
