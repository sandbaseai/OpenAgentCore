package codex

import "testing"

func TestTruncateRunesKeepsRuneBoundaries(t *testing.T) {
	const suffix = "\n…[truncated]"
	for _, test := range []struct {
		text  string
		limit int
		want  string
	}{
		{"héllo世界", 3, "hél" + suffix},
		{"héllo", 5, "héllo"},
		{"héllo", 9, "héllo"},
		{"世界", 0, suffix},
		{"abc", -1, suffix},
		{"", 0, ""},
	} {
		if got := truncateRunes(test.text, test.limit); got != test.want {
			t.Fatalf("truncateRunes(%q, %d) = %q, want %q", test.text, test.limit, got, test.want)
		}
	}
}
