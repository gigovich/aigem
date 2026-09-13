package uisession

import (
	"errors"
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

// "skill:review" is one of a family, and a family is one handler: registering
// a member per skill would go stale the moment the catalogue was reloaded.
func TestCommandFallsBackToThePrefixHandler(t *testing.T) {
	l := New(Config{})
	var got string
	l.Handle("skill:", func(args string) error {
		got = args
		return nil
	})
	if err := l.Command("skill:review", "the diff"); err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got != "review the diff" {
		t.Errorf("the prefix handler was given %q, want the member ahead of the args", got)
	}
	if err := l.Command("skill:review", ""); err != nil || got != "review" {
		t.Errorf("without args: err=%v got=%q", err, got)
	}
	if err := l.Command("other:thing", ""); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("an unregistered family = %v, want ErrUnknownCommand", err)
	}
	l.Handle("mcp__", func(args string) error {
		got = args
		return nil
	})
	if err := l.Command("mcp__srv__prompt", "x"); err != nil || got != "srv__prompt x" {
		t.Errorf("mcp family: err=%v got=%q", err, got)
	}
}
