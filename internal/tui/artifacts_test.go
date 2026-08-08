package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/gigovich/aigem/internal/tools"
)

func TestRecordArtifact(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := Model{reg: reg, artIndex: map[string]*artifact{}}

	a := filepath.Join(reg.Root(), "a.go")
	m.recordArtifact(fileChangeMsg{path: a, old: "", new: "one\n", created: true})
	if len(m.artifacts) != 1 || m.artifacts[0].path != "a.go" || !m.artifacts[0].created {
		t.Fatalf("expected one created artifact a.go, got %+v", m.artifacts)
	}

	// A repeat change to the same file updates cur but keeps the original baseline
	// and does not add a second entry.
	m.recordArtifact(fileChangeMsg{path: a, old: "one\n", new: "two\n"})
	if len(m.artifacts) != 1 {
		t.Fatalf("repeat edit should not add an entry, got %d", len(m.artifacts))
	}
	if got := m.artifacts[0]; got.orig != "" || got.cur != "two\n" {
		t.Fatalf("baseline should stay original, cur should update: %+v", got)
	}

	b := filepath.Join(reg.Root(), "sub", "b.go")
	m.recordArtifact(fileChangeMsg{path: b, old: "x\n", new: "y\n"})
	if len(m.artifacts) != 2 || m.artifacts[1].path != "sub/b.go" || m.artifacts[1].created {
		t.Fatalf("expected second edited artifact sub/b.go, got %+v", m.artifacts)
	}
}

func TestDiffStat(t *testing.T) {
	add, del := diffStat("a\nb\nc\n", "a\nB\nc\nd\n")
	if add != 2 || del != 1 {
		t.Fatalf("expected +2 -1, got +%d -%d", add, del)
	}
	if add, del := diffStat("same\n", "same\n"); add != 0 || del != 0 {
		t.Fatalf("identical content should be +0 -0, got +%d -%d", add, del)
	}
}

func TestRenderDiffSideBySide(t *testing.T) {
	m := &Model{width: 80}
	out := m.renderDiff("a.go", lineDiff("alpha\nbeta\n", "alpha\nGAMMA\n"))
	for i, ln := range strings.Split(out, "\n") {
		if strings.Count(ln, "\n") != 0 {
			t.Fatalf("row %d leaked a newline", i)
		}
	}
	plain := ansiRE.ReplaceAllString(out, "")
	if !strings.Contains(plain, "a.go") || !strings.Contains(plain, "GAMMA") ||
		!strings.Contains(plain, "beta") {
		t.Fatalf("diff should mention path, old and new lines:\n%s", plain)
	}
}

func TestSliceCols(t *testing.T) {
	cases := []struct {
		s      string
		off, w int
		want   string
	}{
		{"abcdef", 0, 3, "abc"},
		{"abcdef", 2, 3, "cde"},
		{"abcdef", 4, 10, "ef"},
		{"abcdef", 6, 3, ""}, // offset at end
		{"abcdef", 9, 3, ""}, // offset past end
		{"abcdef", 0, 0, ""}, // zero width
		{"", 0, 5, ""},
	}
	for _, c := range cases {
		if got := sliceCols(c.s, c.off, c.w); got != c.want {
			t.Errorf("sliceCols(%q,%d,%d) = %q, want %q", c.s, c.off, c.w, got, c.want)
		}
	}
}

// Shifting diffScrollX reveals later columns of a long line in the rendered diff.
func TestRenderDiffHorizontalScroll(t *testing.T) {
	long := "HEAD_" + strings.Repeat("x", 200) + "_TAIL_MARKER"
	ops := lineDiff("a\n", "a\n"+long+"\n")
	m := &Model{width: 120, diffMaxLen: len([]rune(long))}

	plain := func() string { return ansiRE.ReplaceAllString(m.renderDiff("a.go", ops), "") }
	if !strings.Contains(plain(), "HEAD_") {
		t.Fatal("at offset 0 the start of the line should be visible")
	}
	if strings.Contains(plain(), "TAIL_MARKER") {
		t.Fatal("the far end should not be visible at offset 0")
	}
	m.scrollDiff(200)
	if !strings.Contains(plain(), "TAIL_MARKER") {
		t.Fatal("after scrolling right the line tail should be visible")
	}
	if strings.Contains(plain(), "HEAD_") {
		t.Fatal("after scrolling right the line head should be off-screen")
	}
}

// A line longer than a column must be truncated, never wrapped onto a second
// visual row, so the side-by-side grid stays aligned.
func TestRenderDiffTruncatesLongLines(t *testing.T) {
	long := strings.Repeat("x", 500)
	m := &Model{width: 120}
	out := m.renderDiff("a.go", lineDiff("a\n", "a\n"+long+"\n"))
	for i, ln := range strings.Split(out, "\n") {
		if w := lipgloss.Width(ln); w > 120 {
			t.Fatalf("row %d is %d cells wide, exceeds terminal width 120", i, w)
		}
	}
	if strings.Contains(ansiRE.ReplaceAllString(out, ""), strings.Repeat("x", 200)) {
		t.Fatal("long line should be truncated, not rendered in full")
	}
}
