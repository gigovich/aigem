package tui

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
)

// fileItem is one entry in the @-file autocomplete menu.
type fileItem struct {
	path  string // path relative to the project root, slash-separated
	isDir bool
}

// fileMenu is the live fuzzy-filtered file list shown while the user types an
// @path reference anywhere in the input.
type fileMenu struct {
	items  []fileItem
	cursor int
	start  int // byte offset of the '@' that opened the menu
}

// fileMenuMaxMatches caps how many fuzzy matches the menu keeps; the view draws
// at most maxRows of them.
const fileMenuMaxMatches = 50

// activeAtToken finds the @path reference being typed at the end of val: the
// last '@' that starts a word (preceded by start-of-line or whitespace) and is
// followed by a run with no whitespace. It returns the '@' offset and the query
// (text after the '@'). ok is false when no such token is open.
func activeAtToken(val string) (start int, query string, ok bool) {
	at := strings.LastIndex(val, "@")
	if at < 0 {
		return 0, "", false
	}
	if at > 0 {
		switch val[at-1] {
		case ' ', '\t', '\n':
		default:
			return 0, "", false
		}
	}
	query = val[at+1:]
	if strings.ContainsAny(query, " \t\n") {
		return 0, "", false
	}
	return at, query, true
}

// ensureFileIndex walks the project root once and caches every file and
// directory path. The walk is lazy so a session that never types '@' pays
// nothing, and cached so per-keystroke filtering stays cheap.
func (m *Model) ensureFileIndex() {
	if m.filesIndexed {
		return
	}
	m.filesIndexed = true
	if m.reg == nil {
		return
	}
	m.files = listProjectFiles(m.reg.Root())
	m.filePaths = make([]string, len(m.files))
	for i, f := range m.files {
		m.filePaths[i] = f.path
	}
}

// listProjectFiles returns project paths (files and directories) relative to
// root, skipping version-control and dependency directories that would only add
// noise. The scan is bounded so a huge tree cannot stall the UI.
func listProjectFiles(root string) []fileItem {
	const maxScan = 50000
	skipDir := map[string]bool{".git": true, "node_modules": true, "vendor": true}
	var out []fileItem
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && skipDir[d.Name()] && path != root {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		out = append(out, fileItem{path: filepath.ToSlash(rel), isDir: d.IsDir()})
		if len(out) >= maxScan {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// filterFiles ranks the cached index against query. An empty query lists the
// first paths in definition order; otherwise paths are fuzzy-matched and the top
// matches kept.
func (m *Model) filterFiles(query string) []fileItem {
	m.ensureFileIndex()
	if query == "" {
		n := min(len(m.files), fileMenuMaxMatches)
		return append([]fileItem(nil), m.files[:n]...)
	}
	out := make([]fileItem, 0, fileMenuMaxMatches)
	for _, mt := range fuzzy.Find(query, m.filePaths) {
		out = append(out, m.files[mt.Index])
		if len(out) >= fileMenuMaxMatches {
			break
		}
	}
	return out
}

// syncFileMenu opens, updates, or closes the @-file menu based on the current
// input: it shows while an @path token is open and has at least one match.
func (m *Model) syncFileMenu() {
	start, query, ok := activeAtToken(m.input.Value())
	if !ok {
		m.closeFileMenu()
		return
	}
	matches := m.filterFiles(query)
	if len(matches) == 0 {
		m.closeFileMenu()
		return
	}
	if m.fileMenu == nil {
		m.fileMenu = &fileMenu{}
	}
	m.fileMenu.items = matches
	m.fileMenu.start = start
	if m.fileMenu.cursor >= len(matches) {
		m.fileMenu.cursor = len(matches) - 1
	}
	if m.fileMenu.cursor < 0 {
		m.fileMenu.cursor = 0
	}
	m.layout()
}

func (m *Model) closeFileMenu() {
	if m.fileMenu != nil {
		m.fileMenu = nil
		m.layout()
	}
}

// completeFile replaces the open @token with the selected path. A directory
// gains a trailing slash and keeps the menu open so the user can drill in; a
// file is inserted as-is, and on Enter a trailing space closes the menu.
func (m *Model) completeFile(submit bool) {
	menu := m.fileMenu
	it := menu.items[menu.cursor]
	val := m.input.Value()
	repl := "@" + it.path
	switch {
	case it.isDir:
		repl += "/"
	case submit:
		repl += " "
	}
	m.input.SetValue(val[:menu.start] + repl)
	m.input.CursorEnd()
	if it.isDir {
		m.syncFileMenu()
	} else {
		m.closeFileMenu()
	}
}

func (m *Model) handleFileMenuKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	menu := m.fileMenu
	switch msg.Type {
	case tea.KeyUp:
		if menu.cursor > 0 {
			menu.cursor--
		}
		m.refresh()
		return nil, true
	case tea.KeyDown:
		if menu.cursor < len(menu.items)-1 {
			menu.cursor++
		}
		m.refresh()
		return nil, true
	case tea.KeyTab:
		m.completeFile(false)
		return nil, true
	case tea.KeyEnter:
		m.completeFile(true)
		return nil, true
	case tea.KeyEsc:
		m.closeFileMenu()
		return nil, true
	}
	return nil, false
}

func (m Model) fileMenuView() string {
	w := m.overlayInnerWidth()
	const maxRows = 8
	menu := m.fileMenu
	start := 0
	if menu.cursor >= maxRows {
		start = menu.cursor - maxRows + 1
	}
	title := overlayTitleStyle.Render("Files  ") +
		overlayHintStyle.Render("(↑/↓ · Tab complete · Enter insert · Esc close)")
	rows := []string{padLine(title, w, cSurface0)}
	for i := start; i < len(menu.items) && i < start+maxRows; i++ {
		it := menu.items[i]
		label := it.path
		if it.isDir {
			label += "/"
		}
		line := " " + truncate(label, max(8, w-2))
		if i == menu.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}
