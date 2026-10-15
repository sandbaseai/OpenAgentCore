package proto

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestNegotiateUpstreamRecovery(t *testing.T) {
	for _, test := range []struct {
		values   []string
		selected bool
		ok       bool
	}{
		{nil, false, true},
		{[]string{UpstreamRecoveryV1}, true, true},
		{[]string{""}, false, false},
		{[]string{"v2"}, false, false},
		{[]string{UpstreamRecoveryV1, UpstreamRecoveryV1}, false, false},
	} {
		selected, err := NegotiateUpstreamRecovery(test.values)
		if selected != test.selected || (err == nil) != test.ok {
			t.Fatalf("%q: %v %v", test.values, selected, err)
		}
	}
}

func TestUpstreamRecoveryControlsNormalized(t *testing.T) {
	defaults := DefaultUpstreamRecoveryClasses()
	for name, test := range map[string]struct {
		in, want UpstreamRecoveryControls
	}{
		"disabled by default":     {UpstreamRecoveryControls{}, UpstreamRecoveryControls{}},
		"disabled drops the rest": {UpstreamRecoveryControls{MaxAttempts: 2, Classes: []string{RecoveryClassImageInput}}, UpstreamRecoveryControls{}},
		"enabled bare":            {UpstreamRecoveryControls{Enabled: true}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: defaults, TranscriptMaxChars: 120000}},
		"attempts above range":    {UpstreamRecoveryControls{Enabled: true, MaxAttempts: 3}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: defaults, TranscriptMaxChars: 120000}},
		"attempts below range":    {UpstreamRecoveryControls{Enabled: true, MaxAttempts: -1}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: defaults, TranscriptMaxChars: 120000}},
		"two attempts":            {UpstreamRecoveryControls{Enabled: true, MaxAttempts: 2, TranscriptMaxChars: 20000}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 2, Classes: defaults, TranscriptMaxChars: 20000}},
		"budget below range":      {UpstreamRecoveryControls{Enabled: true, TranscriptMaxChars: 19999}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: defaults, TranscriptMaxChars: 120000}},
		"budget above range":      {UpstreamRecoveryControls{Enabled: true, TranscriptMaxChars: 400001}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: defaults, TranscriptMaxChars: 120000}},
		"unknown classes dropped": {UpstreamRecoveryControls{Enabled: true, Classes: []string{"nope", RecoveryClassTrailingAssistant, RecoveryClassTrailingAssistant}}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: []string{RecoveryClassTrailingAssistant}, TranscriptMaxChars: 120000}},
		"only unknown classes":    {UpstreamRecoveryControls{Enabled: true, Classes: []string{"nope"}}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, TranscriptMaxChars: 120000}},
		"stable class order":      {UpstreamRecoveryControls{Enabled: true, Classes: []string{RecoveryClassImageInput, RecoveryClassReasoningState}}, UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: []string{RecoveryClassReasoningState, RecoveryClassImageInput}, TranscriptMaxChars: 120000}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.in.Normalized(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("%+v want %+v", got, test.want)
			}
		})
	}
}

func TestRecoveryClassesCoverFourFingerprintsAndNeverTheFormatOne(t *testing.T) {
	seen := map[string]bool{}
	for _, class := range UpstreamRecoveryClasses() {
		fingerprint, ok := RecoveryClassFingerprint(class)
		if !ok || fingerprint == FingerprintProviderMessagesRequired || seen[fingerprint] {
			t.Fatal(class, fingerprint)
		}
		if _, paired := FingerprintCode(fingerprint); !paired {
			t.Fatal(fingerprint)
		}
		seen[fingerprint] = true
	}
	if len(seen) != 4 {
		t.Fatal(seen)
	}
	for _, class := range DefaultUpstreamRecoveryClasses() {
		if !slices.Contains(UpstreamRecoveryClasses(), class) {
			t.Fatal(class)
		}
	}
}

func recovered() map[string]any {
	return map[string]any{"fingerprint": FingerprintReasoningEncryptedUnverifiable, "action": "rebuild_thread", "attempt": 1, "outcome": "recovered",
		"dropped": map[string]any{"reasoning_items": 3, "images": 0}, "old_native_session_id": "019e0000-0000-7000-8000-000000000001", "new_native_session_id": "019e0000-0000-7000-8000-000000000002"}
}

func aborted() map[string]any {
	return map[string]any{"fingerprint": FingerprintModelNoImageInput, "action": "rebuild_thread", "attempt": 0, "outcome": "aborted", "reason": "current_input_has_images",
		"dropped": map[string]any{"reasoning_items": 0, "images": 0}, "old_native_session_id": "019e0000-0000-7000-8000-000000000001"}
}

func with(entry map[string]any, key string, value any) map[string]any {
	out := map[string]any{}
	for k, v := range entry {
		out[k] = v
	}
	if value == nil {
		delete(out, key)
	} else {
		out[key] = value
	}
	return out
}

func TestParseUpstreamRecoveriesAcceptsValidRecords(t *testing.T) {
	entries, ok := ParseUpstreamRecoveries([]any{recovered(), aborted()})
	if ok {
		t.Fatalf("a recovered record must not precede another: %+v", entries)
	}
	if _, ok := ParseUpstreamRecoveries([]any{aborted(), recovered()}); ok {
		t.Fatal("aborted must be final")
	}
	exhausted := with(recovered(), "outcome", "exhausted")
	entries, ok = ParseUpstreamRecoveries([]any{exhausted, with(with(with(recovered(), "attempt", 2), "old_native_session_id", "019e0000-0000-7000-8000-000000000002"), "new_native_session_id", "019e0000-0000-7000-8000-000000000003")})
	if !ok || len(entries) != 2 || RecoveryAttemptCount(entries) != 2 || entries[0].Dropped.ReasoningItems != 3 {
		t.Fatalf("%+v %v", entries, ok)
	}
	// A value decoded from JSON, as Core receives it.
	var decoded any
	raw, _ := json.Marshal([]any{recovered()})
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if entries, ok := ParseUpstreamRecoveries(decoded); !ok || len(entries) != 1 {
		t.Fatalf("%+v %v", entries, ok)
	}
	second := with(with(with(with(recovered(), "attempt", 2), "fingerprint", FingerprintModelNoImageInput),
		"old_native_session_id", "019e0000-0000-7000-8000-000000000002"), "new_native_session_id", "019e0000-0000-7000-8000-000000000003")
	if entries, ok := ParseUpstreamRecoveries([]any{with(recovered(), "outcome", "exhausted"), second}); !ok || RecoveryAttemptCount(entries) != 2 {
		t.Fatalf("%+v %v", entries, ok)
	}
}

func TestParseUpstreamRecoveriesRejectsMalformedValuesAsAWhole(t *testing.T) {
	long := strings.Repeat("a", 129)
	for name, value := range map[string]any{
		"nil":                      nil,
		"not an array":             recovered(),
		"empty":                    []any{},
		"too many":                 []any{recovered(), with(recovered(), "attempt", 2), aborted()},
		"unknown field":            []any{with(recovered(), "error", "text")},
		"unknown fingerprint":      []any{with(recovered(), "fingerprint", "nope")},
		"format fingerprint":       []any{with(recovered(), "fingerprint", FingerprintProviderMessagesRequired)},
		"unknown action":           []any{with(recovered(), "action", "retry")},
		"missing action":           []any{with(recovered(), "action", nil)},
		"unknown outcome":          []any{with(recovered(), "outcome", "done")},
		"recovered at attempt 0":   []any{with(recovered(), "attempt", 0)},
		"attempt above range":      []any{with(recovered(), "attempt", 3)},
		"attempts out of order":    []any{with(recovered(), "attempt", 2)},
		"negative attempt":         []any{with(recovered(), "attempt", -1)},
		"aborted with attempt":     []any{with(aborted(), "attempt", 1)},
		"aborted without reason":   []any{with(aborted(), "reason", nil)},
		"aborted unknown reason":   []any{with(aborted(), "reason", "because")},
		"aborted with new session": []any{with(aborted(), "new_native_session_id", "019e0000-0000-7000-8000-000000000002")},
		"recovered with reason":    []any{with(recovered(), "reason", "cancelled")},
		"recovered no new session": []any{with(recovered(), "new_native_session_id", nil)},
		"same session":             []any{with(recovered(), "new_native_session_id", "019e0000-0000-7000-8000-000000000001")},
		"session id with space":    []any{with(recovered(), "new_native_session_id", "bad id")},
		"session id with newline":  []any{with(recovered(), "old_native_session_id", "abc\ndef")},
		"session id too long":      []any{with(recovered(), "new_native_session_id", long)},
		"count too large":          []any{with(recovered(), "dropped", map[string]any{"reasoning_items": 1_000_001, "images": 0})},
		"count negative":           []any{with(recovered(), "dropped", map[string]any{"reasoning_items": 0, "images": -1})},
		"count fractional":         []any{with(recovered(), "dropped", map[string]any{"reasoning_items": 0.5, "images": 0})},
		"disconnected chain":       []any{recovered(), with(with(recovered(), "attempt", 2), "new_native_session_id", "019e0000-0000-7000-8000-000000000003")},
		"one bad entry":            []any{recovered(), with(aborted(), "reason", "because")},
		"oversized":                []any{with(recovered(), "old_native_session_id", strings.Repeat("a", 9000))},
	} {
		t.Run(name, func(t *testing.T) {
			if entries, ok := ParseUpstreamRecoveries(value); ok || entries != nil {
				t.Fatalf("accepted %+v", entries)
			}
		})
	}
}

func TestExecutionControlsOmitUpstreamRecoveryUnlessSet(t *testing.T) {
	raw, err := json.Marshal(ExecutionControls{WebSearch: "disabled", TextVerbosity: "medium"})
	if err != nil || strings.Contains(string(raw), "upstream_recovery") {
		t.Fatal(string(raw), err)
	}
	raw, _ = json.Marshal(ExecutionControls{WebSearch: "disabled", TextVerbosity: "medium", UpstreamRecovery: &UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: []string{RecoveryClassReasoningState}, TranscriptMaxChars: 120000}})
	if !strings.Contains(string(raw), `"upstream_recovery":{"enabled":true,"max_attempts":1,"classes":["reasoning_state"],"transcript_max_chars":120000}`) {
		t.Fatal(string(raw))
	}
}
