package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/runtimedevice"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
)

type observedStreamWriter struct {
	header                          http.Header
	body                            bytes.Buffer
	writeErr, flushErr, deadlineErr error
	writes, flushes                 int
	short                           bool
	flushHook                       func()
}

func (w *observedStreamWriter) Header() http.Header { return w.header }
func (w *observedStreamWriter) WriteHeader(int)     {}
func (w *observedStreamWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short {
		return w.body.Write(p[:len(p)-1])
	}
	return w.body.Write(p)
}
func (w *observedStreamWriter) FlushError() error {
	w.flushes++
	if w.flushHook != nil {
		w.flushHook()
	}
	return w.flushErr
}
func (w *observedStreamWriter) SetWriteDeadline(time.Time) error { return w.deadlineErr }

func TestStreamFirstTextWriteFlushFailuresAndReconnectScope(t *testing.T) {
	for _, failure := range []string{"none", "deadline", "write", "flush", "short"} {
		t.Run(failure, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
			t.Cleanup(func() { slog.SetDefault(previous) })
			w := &observedStreamWriter{header: http.Header{}}
			observation := newStreamTextObservation(t.Context(), "session", 42)
			write := openEventStream(w, 200, observation)
			if write == nil {
				t.Fatal("open")
			}
			private := errors.New("SECRET-ERROR")
			switch failure {
			case "deadline":
				w.deadlineErr = private
			case "write":
				w.writeErr = private
			case "flush":
				w.flushErr = private
			case "short":
				w.short = true
			}
			text := "SECRET-TEXT"
			event := v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: "turn", EventID: "event", Delta: &text}
			readAt := time.Now().Add(-time.Millisecond)
			observation.begin(event, 43, readAt)
			err := emitSessionEvent(write, "session", event)
			observation.end(err)
			if (err != nil) != (failure != "none" && failure != "short") {
				t.Fatal(err)
			}
			observation.begin(event, 44, time.Now())
			observation.end(nil)
			if observation.event.Delta != nil {
				t.Fatal("retained body")
			}
			if bytes.Contains(logs.Bytes(), []byte("SECRET")) {
				t.Fatal("sensitive logging")
			}
			rows := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			if len(rows) != 2 {
				t.Fatalf("unbounded logs %s", logs.Bytes())
			}
			var result map[string]any
			if err := json.Unmarshal(rows[1], &result); err != nil {
				t.Fatal(err)
			}
			if result["event_id"] != "event" || result["session_event_sequence"] != float64(43) || result["scope"] != "first_text_per_turn_connection" {
				t.Fatal(result)
			}
			if result["read_at"] != readAt.UTC().Format(time.RFC3339Nano) {
				t.Fatal("read timestamp lost")
			}
			wantWrite, wantFlush := "success", "success"
			switch failure {
			case "deadline":
				wantWrite, wantFlush = "unobserved", "unobserved"
			case "write":
				wantWrite, wantFlush = "error", "unobserved"
			case "flush":
				wantFlush = "error"
			case "short":
				wantWrite = "short_write"
			}
			if result["write_status"] != wantWrite || result["flush_status"] != wantFlush {
				t.Fatal(result)
			}
			if failure == "short" && result["status"] != "incomplete" {
				t.Fatal(result)
			}
			if wantFlush == "unobserved" && result["flush_ms"] != nil {
				t.Fatal("invented flush duration")
			}
			next := newStreamTextObservation(t.Context(), "session", 100)
			if next.connection == observation.connection {
				t.Fatal("reconnect shares identity")
			}
			if failure == "none" && !bytes.Contains(w.body.Bytes(), []byte(`"delta":"SECRET-TEXT"`)) {
				t.Fatal("changed wire")
			}
		})
	}
}

func TestStreamFirstTextIgnoresEmptyNonTextAndKeepsOnlyFirst(t *testing.T) {
	o := newStreamTextObservation(t.Context(), "session", 0)
	blank := ""
	for _, event := range []v1.SessionEvent{{Type: "agent.session.turn.output_text.delta", Delta: &blank}, {Type: "agent.session.turn.reasoning_summary_text.delta", Delta: &blank}, {Type: "agent.session.turn.item.added"}} {
		o.begin(event, 1, time.Now())
	}
	if len(o.seen) != 0 || o.unknownSeen != 0 {
		t.Fatal("non-text observed")
	}
	text := "answer"
	o.begin(v1.SessionEvent{Type: "agent.session.turn.output_text.done", Text: &text}, 2, time.Now())
	if !o.active {
		t.Fatal("non-streamed final missing")
	}
}

func TestStreamTextFlushObservedOnlyAfterReturn(t *testing.T) {
	w := &observedStreamWriter{header: http.Header{}}
	o := newStreamTextObservation(t.Context(), "session", 0)
	write := openEventStream(w, 200, o)
	if write == nil {
		t.Fatal("open")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	w.flushHook = func() { close(entered); <-release }
	text := "answer"
	event := v1.SessionEvent{Type: "agent.session.turn.output_text.delta", EventID: "event", Delta: &text}
	o.begin(event, 1, time.Now())
	done := make(chan error, 1)
	go func() { err := emitSessionEvent(write, "session", event); o.end(err); done <- err }()
	<-entered
	if o.flushStatus != "unobserved" || !o.active {
		t.Fatal("flush observed before return")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if o.flushStatus != "success" || o.active {
		t.Fatal("flush completion missing")
	}
}

func TestStreamTextMultipleTurnsLateEventsMissingIDAndCapacity(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	o := newStreamTextObservation(t.Context(), "session", 0)
	w := &observedStreamWriter{header: http.Header{}}
	write := openEventStream(w, 200, o)
	if write == nil {
		t.Fatal("open")
	}
	text := "text"
	emit := func(turn, eventID string, selected bool) {
		t.Helper()
		event := v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: turn, EventID: eventID, Delta: &text}
		o.begin(event, 1, time.Now())
		if o.active != selected {
			t.Fatalf("turn %q selected=%v", turn, o.active)
		}
		if selected && (o.writeMS != nil || o.flushMS != nil || o.writeStatus != "unobserved" || o.flushStatus != "unobserved") {
			t.Fatal("prior Turn timings retained")
		}
		err := emitSessionEvent(write, "session", event)
		o.end(err)
		if err != nil {
			t.Fatal(err)
		}
	}
	emit("cold", "cold-event", true)
	emit("cold", "duplicate", false)
	emit("warm", "warm-event", true)
	// Replayed lifecycle and late old text never clear the retained identity.
	o.begin(v1.SessionEvent{Type: "agent.session.turn.in_progress", TurnID: "cold"}, 2, time.Now())
	emit("cold", "late-event", false)
	emit("", "unknown-event", true)
	emit("", "unknown-again", false)
	for i := 2; i < streamTextTurnLimit; i++ {
		emit(fmt.Sprintf("turn-%d", i), fmt.Sprintf("event-%d", i), true)
	}
	for i := 0; i < 10; i++ {
		emit(fmt.Sprintf("overflow-%d", i), "overflow-event", false)
	}
	emit("cold", "late-after-limit", false)
	if len(o.seen) != streamTextTurnLimit {
		t.Fatal("unbounded identities")
	}
	if bytes.Count(logs.Bytes(), []byte(`"msg":"session stream first text read"`)) != streamTextTurnLimit+1 || bytes.Count(logs.Bytes(), []byte(`"msg":"session stream first text output"`)) != streamTextTurnLimit+1 || bytes.Count(logs.Bytes(), []byte(`"msg":"session stream text observation limit"`)) != 1 {
		t.Fatal("unbounded or missing observations")
	}
	if !bytes.Contains(w.body.Bytes(), []byte("overflow-event")) {
		t.Fatal("observation limit changed forwarding")
	}
	reconnect := newStreamTextObservation(t.Context(), "session", 100)
	reconnect.begin(v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: "cold", EventID: "later-cold", Delta: &text}, 101, time.Now())
	if !reconnect.active || reconnect.connection == o.connection {
		t.Fatal("reconnect not independently scoped")
	}
}

func TestStreamTerminalAndTextUseIndependentBitsWithinTurnBound(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	o := newStreamTextObservation(t.Context(), "session", 0)
	w := &observedStreamWriter{header: http.Header{}}
	write := openEventStream(w, 200, o)
	if write == nil {
		t.Fatal("open")
	}
	text := "answer"
	emit := func(turn string, terminal, selected bool) {
		t.Helper()
		event := v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: turn, EventID: "actual", Delta: &text}
		if terminal {
			event.Type = "agent.session.turn.completed"
			event.Delta = nil
		}
		o.begin(event, 1, time.Now())
		if o.active != selected {
			t.Fatalf("turn=%q terminal=%v active=%v", turn, terminal, o.active)
		}
		err := emitSessionEvent(write, "session", event)
		o.end(err)
		if err != nil {
			t.Fatal(err)
		}
	}
	emit("terminal-first", true, true)
	emit("terminal-first", false, true)
	emit("terminal-first", true, false)
	emit("terminal-first", false, false)
	emit("", false, true)
	emit("", true, true)
	emit("", true, false)
	emit("", false, false)
	for i := 1; i < streamTextTurnLimit; i++ {
		emit(fmt.Sprintf("turn-%d", i), false, true)
	}
	// A full identity set still admits the other boundary of a retained Turn.
	emit("turn-1", true, true)
	emit("turn-1", true, false)
	emit("new-overflow", true, false)
	if len(o.seen) != streamTextTurnLimit || o.unknownSeen != 3 {
		t.Fatal("bound/unknown bits")
	}
}

func TestStreamLoopPrefixSnapshotAndNoEmptyNull(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	o := newStreamTextObservation(t.Context(), "session", 0)
	now := time.Now()
	o.loop.listed(now, now.Add(time.Millisecond), true, nil)
	o.loop.polled(25 * time.Millisecond)
	o.loop.checked(3 * time.Millisecond)
	o.loop.listed(now.Add(30*time.Millisecond), now.Add(35*time.Millisecond), false, nil)
	text := "PRIVATE"
	o.begin(v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: "turn", EventID: "event", Delta: &text}, 1, now.Add(35*time.Millisecond))
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{"empty_read_count": 1, "poll_wait_count": 1, "poll_wait_total_ms": 25, "authority_check_total_ms": 3, "list_events_count": 2, "list_events_total_ms": 6, "first_text_list_ms": 5, "last_empty_to_first_read_ms": 34} {
		if row[key] != want {
			t.Fatalf("%s=%v want %v", key, row[key], want)
		}
	}
	if bytes.Contains(logs.Bytes(), []byte("PRIVATE")) {
		t.Fatal("text leaked")
	}
	fresh := streamLoopObservation{}
	fields := fresh.fields(now)
	for i := 0; i < len(fields); i += 2 {
		if fields[i] == "last_empty_to_first_read_ms" && fields[i+1] != nil {
			t.Fatal("invented empty read")
		}
	}
	o.loop.cached()
	if o.loop.lastAuthority != nil {
		t.Fatal("cache counted as check")
	}
}

func TestStreamNoTextEndKeepsNullBoundaryAndOutcome(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	o := newStreamTextObservation(t.Context(), "session", 9)
	now := time.Now()
	o.loop.listed(now, now.Add(time.Millisecond), true, nil)
	o.finish("cancelled")
	t.Log("OBSERVATION_FIXTURE", logs.String())
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &row); err != nil {
		t.Fatal(err)
	}
	if row["outcome"] != "cancelled" || row["first_text_list_ms"] != nil || row["last_empty_to_first_read_ms"] != nil || row["poll_wait_total_ms"] != nil {
		t.Fatal(row)
	}
}

func TestStreamLoopActualHandlerPollAndListTimings(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(obslog.NewContextHandler(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	key := callerBinding()
	key.TokenSHA256 = runtimedevice.HashCredential("fixture")
	f := &streamFixture{session: sessions.Session{ID: uuid.NewString(), TenantID: key.TenantID, CreatedAt: time.Now(), Metadata: map[string]string{}, Configuration: json.RawMessage(`{"agent":{"id":"agent_fixture","model":"fixture","tools":[]},"environment":{"type":"none"}}`)}}
	deps, fakes := testDependencies(t)
	f.serve(fakes)
	fakes.projectsReader.resolveAPIKey = projectKeys(t, key).ResolveAPIKey
	calls := 0
	fakes.sessionEvents.listSessionEvents = func(context.Context, string, string, int64) ([]sessions.SessionChange, error) {
		calls++
		time.Sleep(2 * time.Millisecond)
		switch calls {
		case 1:
			return nil, nil
		case 2:
			text := "PRIVATE"
			return []sessions.SessionChange{{Sequence: 11, Event: v1.SessionEvent{Type: "agent.session.turn.output_text.delta", TurnID: "turn", EventID: "event", Delta: &text}}}, nil
		default:
			return nil, sessions.ErrNotFound
		}
	}
	request := httptest.NewRequest("GET", "/v1/agents/sessions/"+f.session.ID+"/events", nil)
	request.Header.Set("Authorization", "Bearer fixture")
	request.Header.Set("OpenAI-Beta", "agents=v1")
	writer := &observedStreamWriter{header: http.Header{}}
	newTestHandler(t, deps).ServeHTTP(writer, request)
	t.Log("OBSERVATION_FIXTURE", logs.String())
	if calls != 3 {
		t.Fatalf("list calls=%d", calls)
	}
	var selected map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "session stream first text read" {
			selected = row
		}
	}
	if selected == nil {
		t.Fatal("missing read")
	}
	if selected["empty_read_count"] != float64(1) || selected["poll_wait_count"] != float64(1) || selected["list_events_count"] != float64(2) || selected["authority_check_count"] != float64(1) {
		t.Fatal(selected)
	}
	if selected["first_text_list_ms"].(float64) < 2 || selected["poll_wait_total_ms"].(float64) < 50 {
		t.Fatal(selected)
	}
	if bytes.Contains(logs.Bytes(), []byte("PRIVATE")) {
		t.Fatal("text leaked")
	}
}
