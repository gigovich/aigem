package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// artifact is one file the agent created or modified this session. orig is the
// content at the first change (empty when the file was created); cur is the
// latest content, so the diff orig->cur is the whole session's change.
type artifact struct {
	path    string // relative to the project root, slash-separated
	orig    string
	cur     string
	created bool
	add     int // cached line stats of orig->cur, refreshed on each change
	del     int
}

// artifactBrowser is the /artifacts picker overlay listing changed files.
type artifactBrowser struct {
	items  []*artifact
	cursor int
}

var (
	cDiffDelBg = lipgloss.Color("#3a2a32")
	cDiffAddBg = lipgloss.Color("#2a3530")

	diffDelStyle = lipgloss.NewStyle().Foreground(cRed).Background(cDiffDelBg)
	diffAddStyle = lipgloss.NewStyle().Foreground(cGreen).Background(cDiffAddBg)
	diffCtxStyle = lipgloss.NewStyle().Foreground(cOverlay1).Background(cCodeBg)
	diffGapStyle = lipgloss.NewStyle().Background(cCodeBg)
	diffSepStyle = lipgloss.NewStyle().Foreground(cOverlay0).Background(cCodeBg)
)

const maxDiffRows = 400

// diffScrollStep is how many columns Shift+Left/Right shifts the diff view.
const diffScrollStep = 12

// scrollDiff shifts the horizontal offset shared by every diff block, clamped so
// it never scrolls past the longest line. It is a no-op when no diff is shown.
func (m *Model) scrollDiff(delta int) {
	if m.diffMaxLen == 0 {
		return
	}
	maxX := max(0, m.diffMaxLen-4)
	m.diffScrollX = max(0, min(m.diffScrollX+delta, maxX))
	m.refresh()
}

// recordArtifact folds a file change into the session artifact list: a repeat
// change to a known file updates its current content (keeping the original
// baseline), a first change appends a new entry.
func (m *Model) recordArtifact(c fileChangeMsg) {
	rel := c.path
	if r, err := filepath.Rel(m.reg.Root(), c.path); err == nil {
		rel = filepath.ToSlash(r)
	}
	if a := m.artIndex[rel]; a != nil {
		a.cur = c.new
		a.add, a.del = diffStat(a.orig, a.cur)
		return
	}
	a := &artifact{path: rel, orig: c.old, cur: c.new, created: c.created}
	a.add, a.del = diffStat(a.orig, a.cur)
	m.artifacts = append(m.artifacts, a)
	m.artIndex[rel] = a
}

func (m *Model) openArtifacts() {
	if len(m.artifacts) == 0 {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no files changed yet this session"})
		m.refresh()
		return
	}
	m.artBr = &artifactBrowser{items: m.artifacts}
	m.layout()
}

func (m *Model) closeArtifacts() {
	if m.artBr != nil {
		m.artBr = nil
		m.layout()
	}
}

func (m *Model) handleArtifactKey(msg tea.KeyMsg) tea.Cmd {
	br := m.artBr
	switch msg.Type {
	case tea.KeyUp:
		if br.cursor > 0 {
			br.cursor--
		}
		m.refresh()
	case tea.KeyDown:
		if br.cursor < len(br.items)-1 {
			br.cursor++
		}
		m.refresh()
	case tea.KeyEnter:
		a := br.items[br.cursor]
		m.closeArtifacts()
		ops := lineDiff(a.orig, a.cur)
		m.blocks = append(m.blocks, block{kind: bkDiff, text: a.path, diffOps: ops})
		for _, o := range ops {
			m.diffMaxLen = max(m.diffMaxLen, len([]rune(o.text)))
		}
		m.refresh()
		m.vp.GotoBottom()
	case tea.KeyEsc:
		m.closeArtifacts()
	}
	return nil
}

func (m Model) artifactBrowserView() string {
	w := m.overlayInnerWidth()
	const maxRows = 8
	br := m.artBr
	start := 0
	if br.cursor >= maxRows {
		start = br.cursor - maxRows + 1
	}
	title := overlayTitleStyle.Render("Artifacts  ") +
		overlayHintStyle.Render("(↑/↓ · Enter view diff · Esc close)")
	rows := []string{padLine(title, w, cSurface0)}
	for i := start; i < len(br.items) && i < start+maxRows; i++ {
		a := br.items[i]
		tag := "edited"
		if a.created {
			tag = "new"
		}
		stat := fmt.Sprintf("+%d -%d %s", a.add, a.del, tag)
		pathW := max(8, w-len(stat)-4)
		line := fmt.Sprintf(" %-*s  %s", pathW, truncate(a.path, pathW), stat)
		if i == br.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

const (
	opEqual = iota
	opDel
	opAdd
)

type diffOp struct {
	kind int
	text string
}

// diffContext is how many unchanged lines to keep around each change.
const diffContext = 3

// lcsCap bounds the LCS table size; past it (after trimming common ends) the
// changed middle is shown as a plain delete-then-add block instead of an
// aligned diff, so a huge file can never blow up memory.
const lcsCap = 2_000_000

// lineDiff returns a line-level edit script transforming old into new, using an
// LCS after trimming the common prefix and suffix. The result keeps unchanged
// lines as opEqual so the renderer can show real context.
func lineDiff(oldText, newText string) []diffOp {
	a, b := diffLines(oldText), diffLines(newText)
	var pre, suf []diffOp
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		pre = append(pre, diffOp{opEqual, a[0]})
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		suf = append([]diffOp{{opEqual, a[len(a)-1]}}, suf...)
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	var mid []diffOp
	if len(a)*len(b) > lcsCap {
		for _, l := range a {
			mid = append(mid, diffOp{opDel, l})
		}
		for _, l := range b {
			mid = append(mid, diffOp{opAdd, l})
		}
	} else {
		mid = lcsDiff(a, b)
	}
	return append(append(pre, mid...), suf...)
}

// lcsDiff is the classic longest-common-subsequence backtrack into del/add/equal ops.
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{opEqual, a[i]})
			i, j = i+1, j+1
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, diffOp{opDel, a[i]})
			i++
		default:
			ops = append(ops, diffOp{opAdd, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{opDel, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{opAdd, b[j]})
	}
	return ops
}

// diffLines splits text into lines, dropping the trailing empty element a final
// newline produces.
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	ls := strings.Split(s, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// sliceCols returns the width-cell window of s starting at column off, used to
// scroll a diff line horizontally. Out-of-range offsets yield an empty window.
func sliceCols(s string, off, width int) string {
	if width < 1 {
		return ""
	}
	r := []rune(s)
	if off >= len(r) {
		return ""
	}
	r = r[off:]
	if len(r) > width {
		r = r[:width]
	}
	return string(r)
}

// diffStat counts inserted and deleted lines between old and new.
func diffStat(oldText, newText string) (add, del int) {
	for _, o := range lineDiff(oldText, newText) {
		switch o.kind {
		case opAdd:
			add++
		case opDel:
			del++
		}
	}
	return add, del
}

// renderDiff lays out the precomputed edit ops as a side-by-side, color-coded
// diff on a solid dark panel: deletions on the left, insertions on the right,
// paired changed lines aligned, and long unchanged runs collapsed to a context
// separator. The LCS is computed once when the block is created, so this is a
// cheap per-frame layout, not a re-diff.
func (m *Model) renderDiff(path string, ops []diffOp) string {
	innerW := max(24, m.width-4)
	leftW := (innerW - 3) / 2
	rightW := innerW - 3 - leftW
	sep := diffSepStyle.Render(" │ ")

	add, del := 0, 0
	for _, o := range ops {
		switch o.kind {
		case opAdd:
			add++
		case opDel:
			del++
		}
	}
	head := diffSepStyle.Bold(true).Render(fmt.Sprintf(" ▌ %s  ", path)) +
		diffAddStyle.Render(fmt.Sprintf("+%d", add)) + diffGapStyle.Render(" ") +
		diffDelStyle.Render(fmt.Sprintf("-%d", del))
	if m.diffMaxLen > leftW-2 { // a line is wider than a column: scrolling helps
		hint := "  ⇧←/⇧→ scroll"
		if m.diffScrollX > 0 {
			hint = fmt.Sprintf("  col %d  ⇧←/⇧→ scroll", m.diffScrollX)
		}
		head += diffSepStyle.Render(hint)
	}
	rows := []string{padLine(head, innerW, cCodeBg)}

	if add == 0 && del == 0 {
		rows = append(rows, padLine(diffCtxStyle.Render("  (no changes)"), innerW, cCodeBg))
		return lipgloss.JoinVertical(lipgloss.Left, rows...)
	}

	// Keep only equal lines within diffContext of a change; the rest are hidden
	// and rendered as a single "N unchanged lines" separator.
	show := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind == opEqual {
			continue
		}
		for j := max(0, i-diffContext); j <= min(len(ops)-1, i+diffContext); j++ {
			show[j] = true
		}
	}

	// The marker stays fixed while the content scrolls horizontally by diffScrollX;
	// each cell is hard-windowed to its column (lipgloss Width would otherwise wrap
	// an over-long line onto a second row and break the side-by-side grid).
	cell := func(st lipgloss.Style, mark, content string, width int) string {
		content = strings.ReplaceAll(strings.TrimRight(content, "\r"), "\t", "    ")
		body := sliceCols(content, m.diffScrollX, width-len([]rune(mark)))
		return st.Width(width).MaxWidth(width).Render(mark + body)
	}
	truncated := 0
	emit := func(s string) {
		if len(rows) <= maxDiffRows {
			rows = append(rows, s)
		} else {
			truncated++
		}
	}
	var dels, adds []string
	flush := func() {
		for i := 0; i < len(dels) || i < len(adds); i++ {
			left, right := cell(diffGapStyle, "", "", leftW), cell(diffGapStyle, "", "", rightW)
			if i < len(dels) {
				left = cell(diffDelStyle, "- ", dels[i], leftW)
			}
			if i < len(adds) {
				right = cell(diffAddStyle, "+ ", adds[i], rightW)
			}
			emit(left + sep + right)
		}
		dels, adds = nil, nil
	}
	for i := 0; i < len(ops); {
		if !show[i] {
			j := i
			for j < len(ops) && !show[j] {
				j++
			}
			flush()
			emit(padLine(diffSepStyle.Render(fmt.Sprintf("  ⋯ %d unchanged line%s",
				j-i, plural(j-i))), innerW, cCodeBg))
			i = j
			continue
		}
		switch ops[i].kind {
		case opEqual:
			flush()
			emit(cell(diffCtxStyle, "  ", ops[i].text, leftW) + sep +
				cell(diffCtxStyle, "  ", ops[i].text, rightW))
		case opDel:
			dels = append(dels, ops[i].text)
		case opAdd:
			adds = append(adds, ops[i].text)
		}
		i++
	}
	flush()
	if truncated > 0 {
		rows = append(rows, padLine(diffCtxStyle.Render(
			fmt.Sprintf("  … %d more line%s (open the file to see the rest)",
				truncated, plural(truncated))), innerW, cCodeBg))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
