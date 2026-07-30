package mattermost

import (
	"strings"
	"testing"
)

func TestSplitForPost(t *testing.T) {
	t.Run("short text stays whole", func(t *testing.T) {
		got := splitForPost("hello world", 100)
		if len(got) != 1 || got[0] != "hello world" {
			t.Fatalf("got %q, want single unchanged chunk", got)
		}
	})

	t.Run("splits on line boundaries under the limit", func(t *testing.T) {
		text := strings.Repeat("line of text\n", 200) // ~2600 runes
		chunks := splitForPost(text, 100)
		if len(chunks) < 2 {
			t.Fatalf("expected multiple chunks, got %d", len(chunks))
		}
		for i, c := range chunks {
			if n := len([]rune(c)); n > 100 {
				t.Fatalf("chunk %d has %d runes, over limit", i, n)
			}
		}
		if joined := strings.Join(chunks, ""); joined != text {
			t.Fatalf("rejoined chunks differ from input")
		}
	})

	t.Run("hard-splits an over-long single line", func(t *testing.T) {
		text := strings.Repeat("x", 250) // one line, no newline
		chunks := splitForPost(text, 100)
		if len(chunks) != 3 {
			t.Fatalf("got %d chunks, want 3", len(chunks))
		}
		for i, c := range chunks {
			if n := len([]rune(c)); n > 100 {
				t.Fatalf("chunk %d has %d runes, over limit", i, n)
			}
		}
		if joined := strings.Join(chunks, ""); joined != text {
			t.Fatalf("rejoined chunks differ from input")
		}
	})

	t.Run("counts runes not bytes", func(t *testing.T) {
		text := strings.Repeat("ё", 120) // 2-byte runes, 120 runes
		chunks := splitForPost(text, 100)
		for i, c := range chunks {
			if n := len([]rune(c)); n > 100 {
				t.Fatalf("chunk %d has %d runes, over limit", i, n)
			}
		}
		if joined := strings.Join(chunks, ""); joined != text {
			t.Fatalf("rejoined chunks differ from input")
		}
	})
}
