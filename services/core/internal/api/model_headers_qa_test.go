package api

import (
	"encoding/json"
	"testing"
)

func TestQASessionModelHeadersStrictRawPath(t *testing.T) {
	for _, headers := range []string{`{"X-A":null}`, `{"X-A":"one","X-A":"two"}`, `{"X-A":"one","x-a":"two"}`, `{"Anthropic-Version":"override"}`} {
		raw := `{"agent":{"model":"fixture"},"environment":{"type":"openai_hosted"},"x_agents_core":{"model_provider":{"protocol":"responses","base_url":"https://model.example/v1","api_key":"fixture","headers":` + headers + `}}}`
		var decoded decodedSessionRequest
		err := json.Unmarshal([]byte(raw), &decoded)
		if err == nil {
			_, err = decoded.validated()
		}
		if err == nil {
			t.Fatal("invalid header input accepted through raw Session decoder")
		}
	}
}
