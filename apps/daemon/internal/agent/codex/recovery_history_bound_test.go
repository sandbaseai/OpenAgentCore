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

func TestDynamicToolCallWithOutcomeButInProgressStatusIsUnsupported(t *testing.T) {
	raw := json.RawMessage(`{"type":"dynamicToolCall","tool":"t","status":"inProgress","success":true}`)
	var stats historyStats
	if got := decodeItem(raw, false, false, &stats); got != nil || stats.unsupported != 1 {
		t.Fatalf("got %v unsupported=%d, want rejected", got, stats.unsupported)
	}
}

func TestBoundedTurnsRejectExcessTurns(t *testing.T) {
	body := `[` + strings.Repeat("{},", maxHistoryTurns) + `{}]`
	var turns boundedTurns
	if err := json.Unmarshal([]byte(body), &turns); !errors.Is(err, errHistoryTooLarge) {
		t.Fatalf("err = %v, want errHistoryTooLarge", err)
	}
}

func TestDecodeUserPartsRejectsExcessBlocks(t *testing.T) {
	raw := json.RawMessage(`{"content":[` + strings.Repeat(`{"type":"text"},`, maxUserContentBlocks) + `{"type":"text"}]}`)
	if parts, ok := decodeUserParts(raw); ok || parts != nil {
		t.Fatalf("want rejection, got ok=%v", ok)
	}
}

func TestCommandWithExitCodeButInProgressStatusIsUnsupported(t *testing.T) {
	raw := json.RawMessage(`{"type":"commandExecution","command":"rm x","exitCode":0,"status":"inProgress"}`)
	var stats historyStats
	if got := decodeItem(raw, false, false, &stats); got != nil || stats.unsupported != 1 {
		t.Fatalf("got %v unsupported=%d, want rejected", got, stats.unsupported)
	}
}
func TestNestedToolArraysRejectExcessElements(t *testing.T) {
	many := func(elem string) string { return `[` + strings.Repeat(elem+",", maxNestedListItems) + elem + `]` }
	items := map[string]string{
		"mcp":     `{"type":"mcpToolCall","server":"s","tool":"t","arguments":{},"status":"completed","result":{"content":` + many(`{"type":"text"}`) + `}}`,
		"dynamic": `{"type":"dynamicToolCall","tool":"t","arguments":{},"status":"completed","success":true,"contentItems":` + many(`{"type":"text"}`) + `}`,
		"file":    `{"type":"fileChange","status":"completed","changes":` + many(`{"path":"a"}`) + `}`,
		"search":  `{"type":"webSearch","status":"completed","query":"q","action":{"type":"search","queries":` + many(`"q"`) + `}}`,
	}
	for name, body := range items {
		var stats historyStats
		if got := decodeItem(json.RawMessage(body), false, false, &stats); got != nil || stats.unsupported != 1 {
			t.Errorf("%s: got %v unsupported=%d, want rejected", name, got, stats.unsupported)
		}
	}
}
func TestNativeTurnBytesChargesTurnOverhead(t *testing.T) {
	if got := nativeTurnBytes(nativeTurn{}); got < historyTurnOverhead {
		t.Fatalf("nativeTurnBytes = %d, want >= %d", got, historyTurnOverhead)
	}
}
func TestDynamicToolCallWithBlankToolIsUnsupported(t *testing.T) {
	raw := json.RawMessage(`{"type":"dynamicToolCall","tool":"  ","arguments":{},"status":"completed","success":true}`)
	var stats historyStats
	if got := decodeItem(raw, false, false, &stats); got != nil || stats.unsupported != 1 {
		t.Fatalf("got %v unsupported=%d, want rejected", got, stats.unsupported)
	}
}
