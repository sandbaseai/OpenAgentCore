package codex

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// Everything below is driven by scrubbed fixtures and an in-memory fake of the
// native app-server. None of it was observed against a live codex 0.153.4:
// the 400 message shapes (design V1), the history item shapes (V2), the
// thread/start parameters (V3), the notification-before-reply ordering (V11) and
// the choice of turn/completed for the failure (V9) are ASSUMPTIONS to be
// calibrated live. The fixtures use fake ids and canary strings only.

const (
	oldThread = "thread-old"
	oldTurn   = "turn-old"
	newThread = "thread-new"
	newTurn   = "turn-new"
	// Later rebuilds in the same Turn get numbered ids.
	secondThread = "thread-new-2"
	secondTurn   = "turn-new-2"

	canaryReasoning = "CANARY_ENCRYPTED_REASONING_STATE"
	canaryImageData = "data:image/png;base64,CANARY_IMAGE_BYTES"
	canarySecret    = "sk-canary-secret"
)

// Scrubbed 400 bodies for each fingerprint; the envelope is codex's native HTTP
// error text.
func upstream400(body string) string {
	return "unexpected status 400 Bad Request: " + body
}

var (
	msgReasoning = upstream400(`{"error":{"message":"The encrypted content for item rs_SCRUBBED could not be verified.","type":"invalid_request_error"}}`)
	msgNoImage   = upstream400(`{"error":{"message":"Model do not support image input","type":"invalid_request_error"}}`)
	msgToolImage = upstream400(`{"message":"messages.3.content.0.tool_result.content.0: Input tag 'input_image' found using 'type' does not match any of the expected tags: 'document', 'image', 'text'"}`)
	msgTrailing  = upstream400(`{"error":{"message":"Requests ending with a model turn are not supported","type":"invalid_request_error"}}`)
	msgMessages  = upstream400(`{"error":{"message":"field messages is required"}}`)
	msgPlain400  = upstream400(`{"error":{"message":"invalid parameter","type":"invalid_request_error"}}`)
)

func failedTurnJSON(threadID, turnID, message string, usage string) json.RawMessage {
	body := map[string]any{"id": turnID, "status": "failed", "error": map[string]any{"message": message, "codexErrorInfo": "other"}}
	if usage != "" {
		body["usage"] = json.RawMessage(usage)
	}
	raw, _ := json.Marshal(map[string]any{"threadId": threadID, "turn": body})
	return raw
}

func completedTurnJSON(threadID, turnID string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "completed"}})
	return raw
}

func item(kind string, fields map[string]any) json.RawMessage {
	out := map[string]any{"type": kind, "id": kind + "-fixture"}
	for k, v := range fields {
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	return raw
}

func userItem(parts ...map[string]any) json.RawMessage {
	content := make([]any, len(parts))
	for i, p := range parts {
		content[i] = p
	}
	return item("userMessage", map[string]any{"content": content})
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
func image() map[string]any        { return map[string]any{"type": "image", "url": canaryImageData} }

// fixtureHistory is a scrubbed thread: one finished Turn and the failed one.
func fixtureHistory(kind string) []historyTurn {
	finished := historyTurn{ID: "turn-1", Status: "completed", Items: []json.RawMessage{
		userItem(text("build the thing")),
		item("reasoning", map[string]any{"summary": []string{canaryReasoning}, "encryptedContent": canaryReasoning}),
		item("agentMessage", map[string]any{"text": "built it"}),
	}}
	failed := historyTurn{ID: oldTurn, Status: "failed", Items: []json.RawMessage{
		userItem(text("please continue")),
		item("commandExecution", map[string]any{"cwd": "/work", "command": "ls", "aggregatedOutput": "a.txt\nb.txt", "exitCode": 0}),
	}}
	switch kind {
	case "reasoning":
		failed.Items = append(failed.Items, item("reasoning", map[string]any{"summary": []string{canaryReasoning}}), item("agentMessage", map[string]any{"text": "listed files"}))
	case "both":
		finished.Items = append([]json.RawMessage{userItem(text("look at this"), image())}, finished.Items...)
	case "history-image":
		finished.Items = append([]json.RawMessage{userItem(text("look at this"), image())}, finished.Items...)
		finished.Items = append(finished.Items[:2], finished.Items[3:]...) // no reasoning
	case "tool-image":
		failed.Items = append(failed.Items, item("dynamicToolCall", map[string]any{"tool": "screenshot", "status": "completed", "success": true, "arguments": map[string]any{}, "contentItems": []any{map[string]any{"type": "inputImage", "imageUrl": canaryImageData}}}))
	case "trailing":
		failed.Items = append(failed.Items, item("agentMessage", map[string]any{"text": "partial answer"}))
	}
	return []historyTurn{finished, failed}
}

type fixtureReader struct {
	turns []historyTurn
	err   error
	// failFrom makes every read after that many calls fail with err.
	failFrom int32
	// block makes ReadHistory wait for the context (or release).
	block chan struct{}
	calls atomic.Int32
}

func (r *fixtureReader) ReadHistory(ctx context.Context, threadID string) ([]historyTurn, error) {
	n := r.calls.Add(1)
	if r.failFrom > 0 && n > r.failFrom {
		return nil, context.DeadlineExceeded
	}
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	turns := r.turns
	// A rebuilt thread's history also lists the Turn that later failed on it.
	switch threadID {
	case newThread:
		turns = append(append([]historyTurn{}, turns...), historyTurn{ID: newTurn, Status: "failed", Items: []json.RawMessage{userItem(text("request"))}})
	case secondThread:
		turns = append(append([]historyTurn{}, turns...), historyTurn{ID: secondTurn, Status: "failed", Items: []json.RawMessage{userItem(text("request"))}})
	}
	return turns, r.err
}

// fakeNative answers the daemon's requests like a codex app-server.
type fakeNative struct {
	t      *testing.T
	server ServerSide
	wmu    sync.Mutex

	mu       sync.Mutex
	requests []JsonRpcRequest

	// hold maps a method to a channel; the reply waits until it is closed, or is
	// never sent when the channel is nil in the map and the method is present.
	hold map[string]chan struct{}
	// fail maps a method to a JSON-RPC error message.
	fail map[string]string
	// before runs ahead of the reply to a method (to model notification-before-reply).
	before map[string]func(JsonRpcRequest)
	// threadModel, when set, is the model thread/start resolves.
	threadModel string
}

func newFakeNative(t *testing.T, server ServerSide) *fakeNative {
	n := &fakeNative{t: t, server: server, hold: map[string]chan struct{}{}, fail: map[string]string{}, before: map[string]func(JsonRpcRequest){}}
	go n.serve()
	return n
}

func (n *fakeNative) write(v any) {
	n.wmu.Lock()
	defer n.wmu.Unlock()
	body, _ := json.Marshal(v)
	_, _ = n.server.ToClient.Write(append(body, '\n'))
}

func (n *fakeNative) notify(method string, params any) {
	n.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (n *fakeNative) serve() {
	decoder := json.NewDecoder(n.server.FromClient)
	for {
		var req JsonRpcRequest
		if decoder.Decode(&req) != nil {
			return
		}
		n.mu.Lock()
		n.requests = append(n.requests, req)
		hold, held := n.hold[req.Method]
		message, failing := n.fail[req.Method]
		before := n.before[req.Method]
		n.mu.Unlock()
		go n.reply(req, hold, held, message, failing, before)
	}
}

func (n *fakeNative) reply(req JsonRpcRequest, hold chan struct{}, held bool, message string, failing bool, before func(JsonRpcRequest)) {
	if before != nil {
		before(req)
	}
	if held {
		if hold == nil {
			return
		}
		<-hold
	}
	if failing {
		n.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": message}})
		return
	}
	var result any = map[string]any{}
	switch req.Method {
	case "thread/start":
		id := newThread
		if n.count("thread/start") > 1 {
			id = secondThread
		}
		model := "synthetic"
		if n.threadModel != "" {
			model = n.threadModel
		}
		result = map[string]any{"thread": map[string]any{"id": id}, "model": model}
	case "turn/start":
		id := newTurn
		if n.count("turn/start") > 1 {
			id = secondTurn
		}
		result = map[string]any{"turn": map[string]any{"id": id}}
	case "turn/steer":
		result = map[string]any{"turnId": newTurn}
	case "thread/backgroundTerminals/list":
		result = map[string]any{"data": []any{}}
	}
	n.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (n *fakeNative) methods() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.requests))
	for i, r := range n.requests {
		out[i] = r.Method
	}
	return out
}

func (n *fakeNative) request(method string) (JsonRpcRequest, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, r := range n.requests {
		if r.Method == method {
			return r, true
		}
	}
	return JsonRpcRequest{}, false
}

func (n *fakeNative) count(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, r := range n.requests {
		if r.Method == method {
			c++
		}
	}
	return c
}

// recoveryFixture is a Turn mid-flight on the old thread, ready for its failure.
type recoveryFixture struct {
	t      *testing.T
	s      *Session
	native *fakeNative
	reader *fixtureReader
	out    chan proto.Envelope
	client *TestClient
}

type fixtureOptions struct {
	controls proto.UpstreamRecoveryControls
	history  []historyTurn
	input    proto.MessageInput
	timeouts recoveryTimeouts
	settle   bool
}

func enabledControls(classes ...string) proto.UpstreamRecoveryControls {
	return proto.UpstreamRecoveryControls{Enabled: true, MaxAttempts: 1, Classes: classes}
}

func newRecoveryFixture(t *testing.T, o fixtureOptions) *recoveryFixture {
	t.Helper()
	client, server, cleanup := NewTestClient()
	t.Cleanup(cleanup)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	out := make(chan proto.Envelope, 256)
	s := &Session{
		executor: &Executor{}, runID: "run", rpc: client.JSONRPCClient, cancelCtx: ctx, cancelFn: cancel, out: out,
		resolvedModel: "synthetic", cfg: sessionConfig{logger: slog.New(slog.DiscardHandler)},
		waitDone: make(chan struct{}), outputDone: make(chan struct{}), cleanup: func() {}, bufs: NewItemBuffers(),
	}
	s.recovery.controls = o.controls.Normalized()
	s.recovery.plan = &SessionPlan{Model: "synthetic", Cwd: "/work", SystemPrompt: "be useful"}
	if o.input == nil {
		o.input = proto.TextInput("please continue")
	}
	s.recovery.input = o.input
	f := &recoveryFixture{t: t, s: s, native: newFakeNative(t, server), reader: &fixtureReader{turns: o.history}, out: out, client: client}
	s.recovery.reader = f.reader
	s.recovery.timeouts = o.timeouts
	s.registerHandlers()
	s.setThreadID(oldThread)
	s.onTurnStarted(json.RawMessage(`{"threadId":"` + oldThread + `","turn":{"id":"` + oldTurn + `"}}`))
	if o.settle {
		go s.settleExecutorTurn(nil)
	}
	return f
}

// fail delivers the failed Turn the way the RPC reader would.
func (f *recoveryFixture) fail(message, usage string) {
	f.t.Helper()
	f.s.onTurnCompleted(failedTurnJSON(oldThread, oldTurn, message, usage))
}

func (f *recoveryFixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *recoveryFixture) acked() bool {
	return f.s.recovery.state.Load() == recoveryIdle && f.s.recovery.attemptCount() > 0
}

// frames collects everything the Session emitted once its output closes.
func (f *recoveryFixture) frames() []proto.Envelope {
	f.t.Helper()
	var frames []proto.Envelope
	timeout := time.After(5 * time.Second)
	for {
		select {
		case env, ok := <-f.out:
			if !ok {
				return frames
			}
			frames = append(frames, env)
		case <-timeout:
			f.t.Fatalf("output did not close; got %d frames", len(frames))
		}
	}
}

// terminal summarizes the terminal frames of a Turn.
type terminal struct {
	errors   []proto.ErrorPayload
	done     proto.DonePayload
	dones    int
	usages   []proto.Usage
	raw      []proto.Envelope
	recovery []proto.UpstreamRecovery
}

func summarize(t *testing.T, frames []proto.Envelope) terminal {
	t.Helper()
	var result terminal
	result.raw = frames
	for _, env := range frames {
		switch env.Type {
		case proto.TypeError:
			var p proto.ErrorPayload
			if err := env.DecodePayload(&p); err != nil {
				t.Fatal(err)
			}
			result.errors = append(result.errors, p)
		case proto.TypeDone:
			result.dones++
			if err := env.DecodePayload(&result.done); err != nil {
				t.Fatal(err)
			}
		case proto.TypeUsage:
			var p proto.UsagePayload
			if err := env.DecodePayload(&p); err != nil {
				t.Fatal(err)
			}
			result.usages = append(result.usages, p.Usage)
		}
	}
	if v, ok := result.done.Metadata[proto.DoneMetaUpstreamRecoveries]; ok {
		records, valid := proto.ParseUpstreamRecoveries(v)
		if !valid {
			t.Fatalf("Core would drop the recovery records: %v", v)
		}
		result.recovery = records
	}
	return result
}

// comparable strips what recovery legitimately adds, leaving what must match the
// run without recovery: the failure text and code, usage and reported thread.
type comparableTerminal struct {
	Errors  []proto.ErrorPayload
	Content string
	Usage   proto.Usage
	Session any
	Dones   int
	Usages  []proto.Usage
}

func (r terminal) comparable() comparableTerminal {
	return comparableTerminal{Errors: r.errors, Content: r.done.Content, Usage: r.done.Usage, Session: r.done.Metadata[proto.DoneMetaAgentSessionID], Dones: r.dones, Usages: r.usages}
}
