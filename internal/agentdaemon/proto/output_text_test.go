package proto

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizeOutputText(t *testing.T) {
	for _, tc := range []struct{ name, kind, input, want string }{
		{"stream", TypeCommandOutput, `{"id":"x","delta":"中\u0000文\\u0000\\x00"}`, `{"id":"x","delta":"中\\x00文\\u0000\\x00"}`},
		{"snapshot", TypeToolCall, `{"args":{"text":"unchanged\u0000"},"result":{"stdout":"a\u0000b","n":9007199254740993},"observation":{"output":["\u0000",{"text":"\u0000"}],"command":"original"}}`, `{"args":{"text":"unchanged\u0000"},"result":{"stdout":"a\\x00b","n":9007199254740993},"observation":{"output":["\\x00",{"text":"\\x00"}],"command":"original"}}`},
		{"input", TypePromptSteer, `{"text":"a\u0000"}`, `{"text":"a\u0000"}`},
		{"literal", TypeCommandOutput, `{"delta":"\\u0000\\x00"}`, `{"delta":"\\u0000\\x00"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := Envelope{Type: tc.kind, ID: "run", Payload: json.RawMessage(tc.input)}
			got, err := NormalizeOutputText(env)
			if err != nil {
				t.Fatal(err)
			}
			canonical := func(raw []byte) string {
				var v any
				d := json.NewDecoder(bytes.NewReader(raw))
				d.UseNumber()
				if err := d.Decode(&v); err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(v)
				return string(b)
			}
			if canonical(got.Payload) != canonical([]byte(tc.want)) {
				t.Fatalf("got %s want %s", got.Payload, tc.want)
			}
			again, err := NormalizeOutputText(got)
			if err != nil || !bytes.Equal(got.Payload, again.Payload) {
				t.Fatalf("not idempotent: %v", err)
			}
			if got.ID != env.ID || got.Type != env.Type {
				t.Fatal("identity changed")
			}
		})
	}
	env, err := NewEnvelope(TypeCommandOutput, "run", CommandOutputPayload{ID: "tool", Delta: "a\x00b"})
	if err != nil {
		t.Fatal(err)
	}
	var out CommandOutputPayload
	if err := env.DecodePayload(&out); err != nil {
		t.Fatal(err)
	}
	if out.Delta != `a\x00b` {
		t.Fatalf("producer output=%q", out.Delta)
	}
}
