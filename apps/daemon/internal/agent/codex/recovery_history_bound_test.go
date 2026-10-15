package codex

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNativeTurnUnmarshalRejectsExcessItems(t *testing.T) {
	body := `{"id":"t","status":"completed","itemsView":"full","items":[` +
		strings.Repeat("{},", maxHistoryItemsPerTurn) + `{}]}`
	var turn nativeTurn
	if err := json.Unmarshal([]byte(body), &turn); !errors.Is(err, errHistoryTooLarge) {
		t.Fatalf("err = %v, want errHistoryTooLarge", err)
	}
}

func TestMCPCallWithOutcomeButInProgressStatusIsUnsupported(t *testing.T) {
	raw := json.RawMessage(`{"type":"mcpToolCall","server":"s","tool":"t","arguments":{},"status":"inProgress","result":{"content":[]}}`)
	var stats historyStats
	if got := decodeItem(raw, false, false, &stats); got != nil || stats.unsupported != 1 {
		t.Fatalf("got %v unsupported=%d, want rejected", got, stats.unsupported)
	}
}

func TestNativeTurnUnmarshalDecodesFields(t *testing.T) {
	var turn nativeTurn
	body := `{"id":"t","status":"completed","extra":{"a":1},"itemsView":"full","items":[{"type":"x"},{}]}`
	if err := json.Unmarshal([]byte(body), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.ID != "t" || turn.Status != "completed" || turn.ItemsView != "full" || len(turn.Items) != 2 {
		t.Fatalf("unexpected turn: %+v", turn)
	}
}
