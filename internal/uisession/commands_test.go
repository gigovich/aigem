package uisession

import (
	"strings"
	"testing"
)

// A bare slash is a request to see everything, in the catalogue's own order.
// Ranking it would put whatever the fuzzy matcher liked first, which is not an
// answer to "what can I do here".
func TestFilterCommandsUnrankedForBareSlash(t *testing.T) {
	cmds := Commands(nil, nil)
	if len(cmds) == 0 {
		t.Fatal("no built-in commands")
	}
	for _, q := range []string{"", "/"} {
		got := FilterCommands(cmds, q)
		if len(got) != len(cmds) || got[0].Name != cmds[0].Name {
			t.Fatalf("FilterCommands(%q) reordered or dropped entries", q)
		}
	}
}

func TestFilterCommandsMatches(t *testing.T) {
	got := FilterCommands(Commands(nil, nil), "/mod")
	if len(got) == 0 || got[0].Name != "/model" {
		t.Fatalf("FilterCommands(\"/mod\") = %+v, want /model first", got)
	}
}

// A description that runs to a paragraph would push the menu open, so it is
// flattened and bounded rather than shown as written.
func TestOneLineBoundsDescription(t *testing.T) {
	got := oneLine("first\nsecond " + strings.Repeat("x", 200))
	if strings.Contains(got, "\n") {
		t.Error("newlines survived")
	}
	if n := len([]rune(got)); n > 101 {
		t.Errorf("description is %d runes; it should be capped", n)
	}
}
