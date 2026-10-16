package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestWorkerFailureLogPreservesCompletionStageWithoutErrorText(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	observeWorkerFailure(t.Context(), "execution_completion", errors.New("confidential-query-canary"))
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event["stage"] != "execution_completion" || event["error_type"] == nil || strings.Contains(output.String(), "confidential-query-canary") {
		t.Fatal("completion failure lost provenance or leaked error text", output.String())
	}
}
