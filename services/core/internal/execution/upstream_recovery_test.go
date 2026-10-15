package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
)

const (
	recoveryOldThread = "019e0000-0000-7000-8000-000000000001"
	recoveryNewThread = "019e0000-0000-7000-8000-000000000002"
)

func recoveryRecord(outcome string) map[string]any {
	return map[string]any{"fingerprint": proto.FingerprintReasoningEncryptedUnverifiable, "action": proto.RecoveryActionRebuildThread, "attempt": 1, "outcome": outcome,
		"dropped": map[string]any{"reasoning_items": 4, "images": 1}, "old_native_session_id": recoveryOldThread, "new_native_session_id": recoveryNewThread}
}

func abortedRecord(reason string) map[string]any {
	return map[string]any{"fingerprint": proto.FingerprintModelNoImageInput, "action": proto.RecoveryActionRebuildThread, "attempt": 0, "outcome": proto.RecoveryOutcomeAborted, "reason": reason,
		"dropped": map[string]any{"reasoning_items": 0, "images": 0}, "old_native_session_id": recoveryOldThread}
}

// abortedAfterSwitch is an aborted record that follows a thread switch, so it starts from the new thread.
func abortedAfterSwitch(reason string) map[string]any {
	record := abortedRecord(reason)
	record["old_native_session_id"] = recoveryNewThread
	return record
}

func doneWithRecoveries(t *testing.T, recoveries any) proto.Envelope {
	t.Helper()
	metadata := map[string]any{proto.DoneMetaAgentSessionID: recoveryNewThread}
	if recoveries != nil {
		metadata[proto.DoneMetaUpstreamRecoveries] = recoveries
	}
	env, err := proto.NewEnvelope(proto.TypeDone, "run", proto.DonePayload{Content: "final", Usage: proto.Usage{InputTokens: 7}, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestDoneRecoveriesAreValidatedStoredAndStrippedFromMetadata(t *testing.T) {
	var result Result
	if err := result.mergeObservation(doneWithRecoveries(t, []any{recoveryRecord(proto.RecoveryOutcomeRecovered)})); err != nil {
		t.Fatal(err)
	}
	if len(result.Recoveries) != 1 || result.recoveriesDropped || proto.RecoveryAttemptCount(result.Recoveries) != 1 || result.Recoveries[0].NewNativeSessionID != recoveryNewThread {
		t.Fatalf("%+v", result)
	}
	if _, present := result.Done.Metadata[proto.DoneMetaUpstreamRecoveries]; present || result.Done.Metadata[proto.DoneMetaAgentSessionID] != recoveryNewThread || result.Done.Content != "final" || result.Done.Usage.InputTokens != 7 {
		t.Fatalf("metadata kept the raw records or lost other values: %+v", result.Done)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Recoveries []proto.UpstreamRecovery `json:"recoveries"`
		Done       struct {
			Metadata map[string]any `json:"metadata"`
		} `json:"done"`
	}
	if err := json.Unmarshal(raw, &persisted); err != nil || len(persisted.Recoveries) != 1 || persisted.Done.Metadata[proto.DoneMetaUpstreamRecoveries] != nil {
		t.Fatal(string(raw), err)
	}
	if persisted.Recoveries[0].Dropped.ReasoningItems != 4 {
		t.Fatal(string(raw))
	}
}

func TestDoneWithoutRecoveriesAddsNothing(t *testing.T) {
	var result Result
	if err := result.mergeObservation(doneWithRecoveries(t, nil)); err != nil || result.Recoveries != nil || result.recoveriesDropped {
		t.Fatal(err, result)
	}
	raw, _ := json.Marshal(result)
	if bytes.Contains(raw, []byte("recoveries")) {
		t.Fatal(string(raw))
	}
}

func TestMalformedRecoveriesAreDroppedWholeAndNeverStored(t *testing.T) {
	const canary = "sk-canary-secret-token"
	extra := recoveryRecord(proto.RecoveryOutcomeExhausted)
	extra["error"] = canary
	for name, value := range map[string]any{
		"unknown field":  []any{extra},
		"too many":       []any{abortedRecord("cancelled"), abortedRecord("cancelled"), abortedRecord("cancelled")},
		"not an array":   map[string]any{"note": canary},
		"string":         canary,
		"unknown reason": []any{abortedRecord(canary)},
		"bad session id": []any{func() map[string]any {
			r := recoveryRecord("recovered")
			r["new_native_session_id"] = canary + " x"
			return r
		}()},
		"one bad of two": []any{recoveryRecord("recovered"), abortedRecord("because")},
		"empty":          []any{},
		"format only": []any{func() map[string]any {
			r := recoveryRecord("recovered")
			r["fingerprint"] = proto.FingerprintProviderMessagesRequired
			return r
		}()},
		"attempt skipped": []any{func() map[string]any { r := recoveryRecord("recovered"); r["attempt"] = 2; return r }()},
	} {
		t.Run(name, func(t *testing.T) {
			var result Result
			if err := result.mergeObservation(doneWithRecoveries(t, value)); err != nil {
				t.Fatal(err)
			}
			if result.Recoveries != nil || !result.recoveriesDropped {
				t.Fatalf("%+v", result)
			}
			raw, _ := json.Marshal(result)
			if bytes.Contains(raw, []byte(canary)) || bytes.Contains(raw, []byte(proto.DoneMetaUpstreamRecoveries)) || bytes.Contains(raw, []byte("recoveries")) {
				t.Fatalf("rejected records reached the outcome: %s", raw)
			}
			// The rest of Done is untouched by the rejection.
			if result.Done.Metadata[proto.DoneMetaAgentSessionID] != recoveryNewThread || result.Done.Content != "final" {
				t.Fatalf("%+v", result.Done)
			}
		})
	}
}

func TestRecoveriesMustMatchReportedNativeSession(t *testing.T) {
	for name, test := range map[string]struct {
		session any
		records []any
	}{
		"switch reports other id": {"019e0000-0000-7000-8000-000000000003", []any{recoveryRecord("recovered")}},
		"switch reports old id":   {recoveryOldThread, []any{recoveryRecord("recovered")}},
		"switch reports none":     {nil, []any{recoveryRecord("recovered")}},
		"abort reports new id":    {recoveryNewThread, []any{abortedRecord("history_unreadable")}},
	} {
		t.Run(name, func(t *testing.T) {
			env := doneWithRecoveries(t, test.records)
			var done proto.DonePayload
			if err := env.DecodePayload(&done); err != nil {
				t.Fatal(err)
			}
			if test.session == nil {
				delete(done.Metadata, proto.DoneMetaAgentSessionID)
			} else {
				done.Metadata[proto.DoneMetaAgentSessionID] = test.session
			}
			env, _ = proto.NewEnvelope(proto.TypeDone, "run", done)
			var result Result
			if err := result.mergeObservation(env); err != nil {
				t.Fatal(err)
			}
			if result.Recoveries != nil || !result.recoveriesDropped {
				t.Fatalf("%+v", result)
			}
		})
	}
	var result Result
	if err := result.mergeObservation(doneWithRecoveries(t, []any{abortedRecord("history_unreadable")})); err != nil {
		t.Fatal(err)
	}
	if result.Recoveries != nil {
		// reports the new id, so must be rejected
		t.Fatalf("%+v", result)
	}
}
func TestRecoveriesSurviveCancellationReceiptAndDrain(t *testing.T) {
	result := Result{ErrorCode: "engine_failed"}
	j := journal{writer: &recoveringWriter{}, next: 1}
	events := make(chan proto.Envelope, 1)
	events <- doneWithRecoveries(t, []any{recoveryRecord(proto.RecoveryOutcomeExhausted)})
	close(events)
	if err := j.drain(events, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Recoveries) != 1 || result.Recoveries[0].Outcome != proto.RecoveryOutcomeExhausted {
		t.Fatalf("drain lost the records: %+v", result)
	}
	// A cancellation outcome without records does not erase what the Turn already reported.
	reply := cancellationResult{ack: proto.InteractionDecisionAckPayload{Applied: true, Outcome: &proto.DonePayload{Usage: proto.Usage{InputTokens: 9}, Metadata: map[string]any{proto.DoneMetaAgentSessionID: recoveryNewThread}}}}
	if err := recordCancellation(context.Background(), &j, reply, &result); err != nil || len(result.Recoveries) != 1 || result.Done.Usage.InputTokens != 9 {
		t.Fatal(err, result)
	}
	// A cancellation outcome that carries records goes through the same validation.
	reply.ack.Outcome.Metadata[proto.DoneMetaUpstreamRecoveries] = []any{abortedAfterSwitch(proto.RecoveryAbortCancelled)}
	if err := recordCancellation(context.Background(), &j, reply, &result); err != nil || len(result.Recoveries) != 1 || result.Recoveries[0].Outcome != proto.RecoveryOutcomeAborted {
		t.Fatal(err, result)
	}
	if _, present := result.Done.Metadata[proto.DoneMetaUpstreamRecoveries]; present {
		t.Fatalf("%+v", result.Done.Metadata)
	}
}

func TestRecoveryCountCountsSwitchesNotAbandonedEntries(t *testing.T) {
	has := func(attrs []any, key string) (any, bool) {
		for i := 0; i+1 < len(attrs); i += 2 {
			if attrs[i] == key {
				return attrs[i+1], true
			}
		}
		return nil, false
	}
	if _, ok := has(engineFailureAttrs(Result{}), "recovery_count"); ok {
		t.Fatal("recovery_count logged without records")
	}
	for name, test := range map[string]struct {
		records []any
		want    int
	}{
		"recovered":      {[]any{recoveryRecord("recovered")}, 1},
		"aborted only":   {[]any{abortedAfterSwitch("history_unreadable")}, 0},
		"aborted second": {[]any{recoveryRecord("exhausted"), abortedAfterSwitch("not_applicable")}, 1},
	} {
		result := Result{ErrorCode: "engine_failed"}
		if name == "recovered" {
			result.ErrorCode = ""
		}
		if err := result.mergeObservation(doneWithRecoveries(t, test.records)); err != nil {
			t.Fatal(name, err)
		}
		if got, ok := has(engineFailureAttrs(result), "recovery_count"); !ok || got != test.want {
			t.Fatalf("%s: %v %v", name, got, ok)
		}
	}
}

func TestTurnCompletionAndRecoveryLogsCarryOnlyEnumerationsAndCounts(t *testing.T) {
	const canary = "sk-canary-secret-token"
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var result Result
	result.Error = "unexpected status 400 Bad Request: " + canary
	if err := result.mergeObservation(doneWithRecoveries(t, []any{recoveryRecord("recovered")})); err != nil {
		t.Fatal(err)
	}
	observeUpstreamRecoveries(context.Background(), "session", "turn", result)
	observeTerminalCommit(context.Background(), "session", "turn", sessions.TurnCompleted, sessions.TurnCompleted, 1, time.Now(), "event", nil, result)

	var recovery, completion map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		switch {
		case row["msg"] == "executor upstream recovery":
			recovery = row
		case row["stage"] == "turn_completion":
			completion = row
		}
	}
	if recovery == nil || recovery["level"] != "INFO" || recovery["fingerprint"] != proto.FingerprintReasoningEncryptedUnverifiable || recovery["action"] != "rebuild_thread" ||
		recovery["attempt"] != float64(1) || recovery["outcome"] != "recovered" || recovery["session_id"] != "session" || recovery["turn_id"] != "turn" {
		t.Fatalf("%v", recovery)
	}
	for _, forbidden := range []string{recoveryOldThread, recoveryNewThread, "reasoning_items"} {
		if _, found := recovery[forbidden]; found {
			t.Fatal(forbidden)
		}
	}
	if completion == nil || completion["recovery_count"] != float64(1) {
		t.Fatalf("%v", completion)
	}
	if strings.Contains(out.String(), canary) || strings.Contains(out.String(), "unexpected status") || strings.Contains(out.String(), recoveryNewThread) {
		t.Fatalf("native text or a native Session ID reached the log: %s", out.String())
	}

	out.Reset()
	observeUpstreamRecoveries(context.Background(), "session", "turn", Result{recoveriesDropped: true})
	if !strings.Contains(out.String(), "executor upstream recovery records dropped") || !strings.Contains(out.String(), `"level":"DEBUG"`) || strings.Contains(out.String(), "executor upstream recovery\"") {
		t.Fatal(out.String())
	}
	out.Reset()
	observeUpstreamRecoveries(context.Background(), "session", "turn", Result{})
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
}

func TestUpstreamRecoveryControlsReachOnlyASelectingPeer(t *testing.T) {
	policy := proto.UpstreamRecoveryControls{Enabled: true, MaxAttempts: 2, Classes: []string{proto.RecoveryClassImageInput}, TranscriptMaxChars: 50000}
	newRequest := func() *proto.PromptRequestPayload {
		return &proto.PromptRequestPayload{AgentKind: "codex", ExecutionControls: &proto.ExecutionControls{WebSearch: "disabled", TextVerbosity: "medium"}}
	}
	for name, test := range map[string]struct {
		policy   proto.UpstreamRecoveryControls
		selected bool
		sent     bool
	}{
		"selected and enabled":      {policy, true, true},
		"peer did not select":       {policy, false, false},
		"disabled":                  {proto.UpstreamRecoveryControls{MaxAttempts: 2, Classes: policy.Classes}, true, false},
		"disabled and not selected": {proto.UpstreamRecoveryControls{}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			req := newRequest()
			withUpstreamRecovery(req, test.policy, test.selected)
			if sent := req.ExecutionControls.UpstreamRecovery != nil; sent != test.sent {
				t.Fatalf("sent=%v", sent)
			}
			raw, _ := json.Marshal(req)
			if !test.sent && bytes.Contains(raw, []byte("upstream_recovery")) {
				t.Fatalf("a peer that cannot take the block received it: %s", raw)
			}
			if test.sent && !reflect.DeepEqual(*req.ExecutionControls.UpstreamRecovery, test.policy.Normalized()) {
				t.Fatalf("%+v", req.ExecutionControls.UpstreamRecovery)
			}
		})
	}
	// The block never aliases Core's own policy.
	req := newRequest()
	withUpstreamRecovery(req, policy, true)
	req.ExecutionControls.UpstreamRecovery.Classes[0] = "mutated"
	if policy.Classes[0] != proto.RecoveryClassImageInput {
		t.Fatal("request shares the class slice with Core's policy")
	}
	// A non-codex Harness on a selecting Runtime never receives the block.
	other := newRequest()
	other.AgentKind = "claude-sdk"
	withUpstreamRecovery(other, policy, true)
	if other.ExecutionControls.UpstreamRecovery != nil {
		t.Fatal("non-codex harness received the block")
	}
	// A multi-agent Turn is rejected by the daemon, so it never receives the block.
	multi := newRequest()
	multi.ObserveSubagentIdentities = true
	withUpstreamRecovery(multi, policy, true)
	if multi.ExecutionControls.UpstreamRecovery != nil {
		t.Fatal("multi-agent turn received the block")
	}
	// A request without controls is left alone.
	bare := &proto.PromptRequestPayload{}
	withUpstreamRecovery(bare, policy, true)
	if bare.ExecutionControls != nil {
		t.Fatal("controls were created for a request that had none")
	}
	// A nil peer is not a selecting peer.
	d := Dispatcher{UpstreamRecovery: policy}
	req = newRequest()
	d.applyUpstreamRecovery(req, nil)
	if req.ExecutionControls.UpstreamRecovery != nil {
		t.Fatal("nil peer received the block")
	}
}

func TestParseUpstreamRecoveriesRejectsAbortedWithoutOldSession(t *testing.T) {
	record := abortedRecord(proto.RecoveryAbortCancelled)
	delete(record, "old_native_session_id")
	if _, ok := proto.ParseUpstreamRecoveries([]any{record}); ok {
		t.Fatal("aborted record without old_native_session_id accepted")
	}
}

func TestRecoveredRecordAfterTerminalErrorIsDropped(t *testing.T) {
	result := Result{ErrorCode: "engine_failed"}
	if err := result.mergeObservation(doneWithRecoveries(t, []any{recoveryRecord(proto.RecoveryOutcomeRecovered)})); err != nil {
		t.Fatal(err)
	}
	if result.Recoveries != nil || !result.recoveriesDropped {
		t.Fatalf("recovered record contradicting a terminal error was kept: %+v", result)
	}
	exhausted := Result{ErrorCode: "engine_failed"}
	if err := exhausted.mergeObservation(doneWithRecoveries(t, []any{recoveryRecord(proto.RecoveryOutcomeExhausted)})); err != nil || len(exhausted.Recoveries) != 1 {
		t.Fatal(err, exhausted)
	}
}
func TestSuccessfulDoneRejectsUnsuccessfulFinalRecovery(t *testing.T) {
	for _, outcome := range []string{proto.RecoveryOutcomeExhausted} {
		var result Result
		if err := result.mergeObservation(doneWithRecoveries(t, []any{recoveryRecord(outcome)})); err != nil {
			t.Fatal(err)
		}
		if result.Recoveries != nil || !result.recoveriesDropped {
			t.Fatalf("%s accepted on a successful Turn: %+v", outcome, result)
		}
	}
	var result Result
	if err := result.mergeObservation(doneWithRecoveries(t, []any{recoveryRecord(proto.RecoveryOutcomeRecovered)})); err != nil || len(result.Recoveries) != 1 {
		t.Fatal(err, result)
	}
}
func TestRecoveriesMustStayWithinSelectedPolicy(t *testing.T) {
	enabled := func(attempts int, classes ...string) *proto.UpstreamRecoveryControls {
		policy := proto.UpstreamRecoveryControls{Enabled: true, MaxAttempts: attempts, Classes: classes}.Normalized()
		return &policy
	}
	second := recoveryRecord(proto.RecoveryOutcomeRecovered)
	second["attempt"] = 2
	second["old_native_session_id"] = recoveryOldThread
	first := recoveryRecord(proto.RecoveryOutcomeExhausted)
	first["new_native_session_id"] = "019e0000-0000-7000-8000-000000000003"
	second["old_native_session_id"] = "019e0000-0000-7000-8000-000000000003"
	for name, test := range map[string]struct {
		policy  *proto.UpstreamRecoveryControls
		records []any
		keep    bool
	}{
		"within policy":        {enabled(2), []any{first, second}, true},
		"over attempt budget":  {enabled(1), []any{first, second}, false},
		"disabled class":       {enabled(2, proto.RecoveryClassImageInput), []any{recoveryRecord(proto.RecoveryOutcomeRecovered)}, false},
		"recovery not offered": {&proto.UpstreamRecoveryControls{}, []any{recoveryRecord(proto.RecoveryOutcomeRecovered)}, false},
	} {
		result := Result{recoveryPolicy: test.policy}
		if err := result.mergeObservation(doneWithRecoveries(t, test.records)); err != nil {
			t.Fatal(name, err)
		}
		if kept := result.Recoveries != nil; kept != test.keep || result.recoveriesDropped == test.keep {
			t.Fatalf("%s: %+v", name, result)
		}
	}
}
