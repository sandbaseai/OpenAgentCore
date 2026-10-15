package codex

import "testing"

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
