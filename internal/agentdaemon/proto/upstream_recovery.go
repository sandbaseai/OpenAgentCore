package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
)

// Upstream recovery is an additive extension. A Runtime that can rebuild a
// native thread after a recoverable upstream HTTP 400 offers exactly one
// X-Oac-Upstream-Recovery: v1 header in the WebSocket upgrade; Core selects it by
// returning the same header. Only a connection that selected it receives
// ExecutionControls.UpstreamRecovery. An absent offer keeps the base protocol, so
// updating Core alone never disconnects an existing Runtime.
const (
	UpstreamRecoveryHeader = "X-Oac-Upstream-Recovery"
	UpstreamRecoveryV1     = "v1"
)

// NegotiateUpstreamRecovery validates one optional upgrade offer or selection.
// Absence explicitly selects the base protocol.
func NegotiateUpstreamRecovery(values []string) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	if len(values) == 1 && values[0] == UpstreamRecoveryV1 {
		return true, nil
	}
	return false, errors.New("unsupported upstream recovery selection")
}

// Recovery classes Core may allow. Each names the one fingerprint it covers.
// The provider_messages_required fingerprint is classified only and has no class.
const (
	RecoveryClassReasoningState    = "reasoning_state"
	RecoveryClassImageInput        = "image_input"
	RecoveryClassToolResultImage   = "tool_result_image"
	RecoveryClassTrailingAssistant = "trailing_assistant"
)

// RecoveryActionRebuildThread starts a fresh native thread from a rendered
// transcript of the previous one, within the same Turn.
const RecoveryActionRebuildThread = "rebuild_thread"

// Recovery outcomes. Recovered and exhausted entries record a completed thread
// switch (attempt >= 1); an aborted entry records a recovery given up before
// the switch (attempt 0) with one of the reasons below.
const (
	RecoveryOutcomeRecovered = "recovered"
	RecoveryOutcomeExhausted = "exhausted"
	RecoveryOutcomeAborted   = "aborted"
)

// Reasons an attempt was abandoned before the new thread accepted its first Turn.
const (
	RecoveryAbortNotApplicable         = "not_applicable"
	RecoveryAbortCurrentInputHasImages = "current_input_has_images"
	RecoveryAbortHistoryUnreadable     = "history_unreadable"
	RecoveryAbortThreadStartFailed     = "thread_start_failed"
	RecoveryAbortTurnStartFailed       = "turn_start_failed"
	RecoveryAbortCancelled             = "cancelled"
	RecoveryAbortModelChanged          = "model_changed"
)

// DoneMetaUpstreamRecoveries is the Done.metadata key that carries the Turn's
// recovery records.
const DoneMetaUpstreamRecoveries = "upstream_recoveries"

// Bounds Core applies to its policy and to the records a Runtime reports.
const (
	MaxUpstreamRecoveries                  = 2
	MinUpstreamRecoveryAttempts            = 1
	MaxUpstreamRecoveryAttempts            = 2
	DefaultUpstreamRecoveryAttempts        = 1
	MinUpstreamRecoveryTranscriptChars     = 20000
	MaxUpstreamRecoveryTranscriptChars     = 400000
	DefaultUpstreamRecoveryTranscriptChars = 120000
	maxRecoveryCount                       = 1_000_000
	maxRecoveryNativeSessionIDLength       = 128
	maxUpstreamRecoveriesPayloadBytes      = 8 << 10
)

var recoveryClassFingerprint = map[string]string{
	RecoveryClassReasoningState:    FingerprintReasoningEncryptedUnverifiable,
	RecoveryClassImageInput:        FingerprintModelNoImageInput,
	RecoveryClassToolResultImage:   FingerprintToolResultInputImageRejected,
	RecoveryClassTrailingAssistant: FingerprintTrailingModelTurn,
}

var recoveryAbortReasons = []string{
	RecoveryAbortNotApplicable, RecoveryAbortCurrentInputHasImages, RecoveryAbortHistoryUnreadable,
	RecoveryAbortThreadStartFailed, RecoveryAbortTurnStartFailed, RecoveryAbortCancelled, RecoveryAbortModelChanged,
}

var recoveryNativeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// UpstreamRecoveryClasses lists every class in a stable order.
func UpstreamRecoveryClasses() []string {
	return []string{RecoveryClassReasoningState, RecoveryClassImageInput, RecoveryClassToolResultImage, RecoveryClassTrailingAssistant}
}

// DefaultUpstreamRecoveryClasses are the classes enabled when none is named. The
// tool-result-image and trailing-assistant classes stay off until observed.
func DefaultUpstreamRecoveryClasses() []string {
	return []string{RecoveryClassReasoningState, RecoveryClassImageInput}
}

// RecoveryClassFingerprint returns the one fingerprint a class covers.
func RecoveryClassFingerprint(class string) (string, bool) {
	fingerprint, ok := recoveryClassFingerprint[class]
	return fingerprint, ok
}

// RecoveryAbortReasons lists the enumerated reasons of an aborted entry.
func RecoveryAbortReasons() []string { return slices.Clone(recoveryAbortReasons) }

// UpstreamRecoveryControls is Core's recovery policy for one execution. Core
// reads the process settings and sends this block; a Runtime never reads its
// own environment for it. Omitting the block, or Enabled false, disables recovery.
type UpstreamRecoveryControls struct {
	Enabled            bool     `json:"enabled"`
	MaxAttempts        int      `json:"max_attempts,omitempty"`
	Classes            []string `json:"classes,omitempty"`
	TranscriptMaxChars int      `json:"transcript_max_chars,omitempty"`
}

// Normalized returns the policy a Runtime applies. Out-of-range or unknown
// values never widen what Core allowed: attempts outside 1 to 2 and a transcript
// budget outside 20000 to 400000 characters fall back to their defaults, unknown
// classes are dropped, and an empty class list means the default classes.
func (c UpstreamRecoveryControls) Normalized() UpstreamRecoveryControls {
	if !c.Enabled {
		return UpstreamRecoveryControls{}
	}
	out := UpstreamRecoveryControls{Enabled: true, MaxAttempts: c.MaxAttempts, TranscriptMaxChars: c.TranscriptMaxChars}
	if out.MaxAttempts < MinUpstreamRecoveryAttempts || out.MaxAttempts > MaxUpstreamRecoveryAttempts {
		out.MaxAttempts = DefaultUpstreamRecoveryAttempts
	}
	if out.TranscriptMaxChars < MinUpstreamRecoveryTranscriptChars || out.TranscriptMaxChars > MaxUpstreamRecoveryTranscriptChars {
		out.TranscriptMaxChars = DefaultUpstreamRecoveryTranscriptChars
	}
	if len(c.Classes) == 0 {
		out.Classes = DefaultUpstreamRecoveryClasses()
		return out
	}
	for _, class := range UpstreamRecoveryClasses() {
		if slices.Contains(c.Classes, class) {
			out.Classes = append(out.Classes, class)
		}
	}
	return out
}

// RecoveryDropped counts what a rebuilt transcript left out.
type RecoveryDropped struct {
	ReasoningItems int `json:"reasoning_items"`
	Images         int `json:"images"`
}

// UpstreamRecovery is one recovery record of a Turn: enumerations, counts and
// native Session IDs only, never message text.
type UpstreamRecovery struct {
	Fingerprint        string          `json:"fingerprint"`
	Action             string          `json:"action"`
	Attempt            int             `json:"attempt"`
	Outcome            string          `json:"outcome"`
	Reason             string          `json:"reason,omitempty"`
	Dropped            RecoveryDropped `json:"dropped"`
	OldNativeSessionID string          `json:"old_native_session_id,omitempty"`
	NewNativeSessionID string          `json:"new_native_session_id,omitempty"`
}

// wireUpstreamRecovery mirrors UpstreamRecovery with pointers on the fields
// whose zero value is meaningful, so a missing field is distinguishable.
type wireUpstreamRecovery struct {
	Fingerprint string `json:"fingerprint"`
	Action      string `json:"action"`
	Attempt     *int   `json:"attempt"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	Dropped     *struct {
		ReasoningItems *int `json:"reasoning_items"`
		Images         *int `json:"images"`
	} `json:"dropped"`
	OldNativeSessionID string `json:"old_native_session_id,omitempty"`
	NewNativeSessionID string `json:"new_native_session_id,omitempty"`
}

// ParseUpstreamRecoveries validates Done.metadata.upstream_recoveries. It
// accepts at most two entries with known fields only; any malformed entry
// rejects the whole value, because a partial record could misstate what ran.
func ParseUpstreamRecoveries(value any) ([]UpstreamRecovery, bool) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxUpstreamRecoveriesPayloadBytes {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire []wireUpstreamRecovery
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	entries := make([]UpstreamRecovery, 0, len(wire))
	for _, w := range wire {
		// Zero is a meaningful value for these counters, so absence must be
		// rejected rather than read as zero.
		if w.Attempt == nil || w.Dropped == nil || w.Dropped.ReasoningItems == nil || w.Dropped.Images == nil {
			return nil, false
		}
		entries = append(entries, UpstreamRecovery{
			Fingerprint: w.Fingerprint, Action: w.Action, Attempt: *w.Attempt, Outcome: w.Outcome, Reason: w.Reason,
			Dropped:            RecoveryDropped{ReasoningItems: *w.Dropped.ReasoningItems, Images: *w.Dropped.Images},
			OldNativeSessionID: w.OldNativeSessionID, NewNativeSessionID: w.NewNativeSessionID,
		})
	}
	if len(entries) == 0 || len(entries) > MaxUpstreamRecoveries {
		return nil, false
	}
	attempts := 0
	previousNew := ""
	seen := map[string]bool{}
	usedIDs := map[string]bool{}
	for i, entry := range entries {
		if !entry.valid() {
			return nil, false
		}
		// Only the last record may be aborted or recovered; starting another
		// attempt leaves every earlier one exhausted.
		if i < len(entries)-1 && entry.Outcome != RecoveryOutcomeExhausted {
			return nil, false
		}
		// Any record after a switch, including an abort, starts from the thread
		// that switch produced.
		if previousNew != "" && entry.OldNativeSessionID != previousNew {
			return nil, false
		}
		// The adapter permits each fingerprint one recovery admission per Turn,
		// aborted ones included.
		if seen[entry.Fingerprint] {
			return nil, false
		}
		seen[entry.Fingerprint] = true
		// A thread already used as a source or target cannot be reused.
		if usedIDs[entry.OldNativeSessionID] && entry.OldNativeSessionID != previousNew {
			return nil, false
		}
		if entry.NewNativeSessionID != "" && usedIDs[entry.NewNativeSessionID] {
			return nil, false
		}
		usedIDs[entry.OldNativeSessionID] = true
		if entry.NewNativeSessionID != "" {
			usedIDs[entry.NewNativeSessionID] = true
		}
		if entry.Attempt != 0 {
			attempts++
			if entry.Attempt != attempts {
				return nil, false
			}
			previousNew = entry.NewNativeSessionID
		}
	}
	return entries, true
}

func (r UpstreamRecovery) valid() bool {
	if _, ok := fingerprintCode[r.Fingerprint]; !ok || r.Fingerprint == FingerprintProviderMessagesRequired {
		return false
	}
	if r.Action != RecoveryActionRebuildThread || r.Attempt < 0 || r.Attempt > MaxUpstreamRecoveryAttempts {
		return false
	}
	if r.Dropped.ReasoningItems < 0 || r.Dropped.ReasoningItems > maxRecoveryCount || r.Dropped.Images < 0 || r.Dropped.Images > maxRecoveryCount {
		return false
	}
	if !validRecoverySessionID(r.OldNativeSessionID) || !validRecoverySessionID(r.NewNativeSessionID) {
		return false
	}
	switch r.Outcome {
	case RecoveryOutcomeAborted:
		// Abandoned before the switch: no new thread exists.
		return r.Attempt == 0 && r.OldNativeSessionID != "" && r.NewNativeSessionID == "" && slices.Contains(recoveryAbortReasons, r.Reason)
	case RecoveryOutcomeRecovered, RecoveryOutcomeExhausted:
		return r.Attempt >= 1 && r.Reason == "" && r.OldNativeSessionID != "" && r.NewNativeSessionID != "" && r.OldNativeSessionID != r.NewNativeSessionID
	}
	return false
}

func validRecoverySessionID(id string) bool {
	return id == "" || (len(id) <= maxRecoveryNativeSessionIDLength && recoveryNativeSessionID.MatchString(id))
}

// RecoveryAttemptCount counts completed thread switches, not abandoned entries.
func RecoveryAttemptCount(entries []UpstreamRecovery) int {
	count := 0
	for _, entry := range entries {
		if entry.Attempt >= 1 {
			count++
		}
	}
	return count
}
