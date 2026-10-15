package codex

import (
	"encoding/json"
	"testing"
)

func TestNativeTurnsBytesChargesPerItemOverhead(t *testing.T) {
	items := make([]json.RawMessage, 1000)
	for i := range items {
		items[i] = json.RawMessage(`{}`)
	}
	got := nativeTurnBytes(nativeTurn{ID: "t", Items: items})
	if want := 1000 * (2 + historyItemOverhead); got < want {
		t.Fatalf("bytes = %d, want >= %d", got, want)
	}
}

func TestNativeTurnDecodeTracksItemsPresence(t *testing.T) {
	for body, want := range map[string]bool{
		`{"id":"a","itemsView":"full"}`:                   false,
		`{"id":"a","itemsView":"full","items":null}`:      false,
		`{"id":"a","itemsView":"full","items":[]}`:        true,
		`{"id":"a","itemsView":"full","items":[{"a":1}]}`: true,
	} {
		var turn nativeTurn
		if err := json.Unmarshal([]byte(body), &turn); err != nil {
			t.Fatal(err)
		}
		if turn.itemsPresent != want {
			t.Fatalf("%s: itemsPresent = %v, want %v", body, turn.itemsPresent, want)
		}
	}
}
