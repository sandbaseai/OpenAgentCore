package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventByteAccountingMatchesWireNormalization(t *testing.T) {
	env := Envelope{Type: TypeDelta, ID: "run", Payload: json.RawMessage(`{ "delta": "<tag>&text", "sequence": 1 }`)}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var received Envelope
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	if before, after := ExecutionEventBytes(env), ExecutionEventBytes(received); before != after {
		t.Fatalf("wire normalization changed credit: %d != %d", before, after)
	}
}

func TestMaximumWireFrameFitsExecutionWindow(t *testing.T) {
	env := Envelope{Type: TypeDelta, ID: "run", Payload: json.RawMessage(`{"delta":""}`)}
	empty, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Payload = json.RawMessage(`{"delta":"` + strings.Repeat("x", MaxFrameBytes-len(empty)) + `"}`)
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != MaxFrameBytes {
		t.Fatalf("frame size %d", len(wire))
	}
	if got := ExecutionEventBytes(env); got > ExecutionEventWindowBytes {
		t.Fatalf("maximum legal frame requires %d credits, window %d", got, ExecutionEventWindowBytes)
	}
}
