package codex

import (
	"encoding/json"
	"testing"
)

func TestInlineHistoryCompleteRequiresFullItems(t *testing.T) {
	full := nativeTurn{ID: "t1", ItemsView: "full"}
	for name, turns := range map[string][]nativeTurn{
		"empty":      nil,
		"shell":      {{ID: "t1", Status: "failed"}},
		"summary":    {full, {ID: "t2", ItemsView: "summary"}},
		"missing id": {{ItemsView: "full"}},
		"not-loaded": {{ID: "t1", ItemsView: "notLoaded"}},
	} {
		if inlineHistoryComplete(turns) {
			t.Fatalf("%s accepted", name)
		}
	}
	if !inlineHistoryComplete([]nativeTurn{full, {ID: "t2", ItemsView: "full"}}) {
		t.Fatal("full turns rejected")
	}
}

func TestNativeTurnsBytesChargesPerItemOverhead(t *testing.T) {
	items := make([]json.RawMessage, 1000)
	for i := range items {
		items[i] = json.RawMessage(`{}`)
	}
	got := nativeTurnsBytes([]nativeTurn{{ID: "t", Items: items}})
	if want := 1000 * (2 + historyItemOverhead); got < want {
		t.Fatalf("bytes = %d, want >= %d", got, want)
	}
}
