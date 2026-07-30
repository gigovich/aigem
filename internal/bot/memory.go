package bot

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	indexFile  = "MEMORY.md"
	archiveDir = "archive"
)

// Fact is one memory entry's frontmatter: index metadata plus usage tracking that the
// daily memory review reads to judge staleness.
type Fact struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Modified    string `yaml:"modified,omitempty"`
	Used        string `yaml:"used,omitempty"`
	Uses        int    `yaml:"uses,omitempty"`
}

// Store is a bot's persistent memory: one markdown file per fact (YAML frontmatter + body)
// under dir, plus a generated MEMORY.md index. Methods are safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex
	now func() time.Time
}

// NewStore binds a memory store to a directory (created lazily on first write).
func NewStore(dir string) *Store { return &Store{dir: dir, now: time.Now} }

func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339) }

// After the closing --- only same-line whitespace and one newline belong to the delimiter;
// a greedy \s* would swallow the body's leading blank lines and indentation, which Read's
// usage-stamp rewrite would then persist.
var frontmatterRe = regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---[ \t]*\n?(.*)$`)

func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Store) path(name string) (string, error) {
	slug := slugify(name)
	if slug == "" {
		return "", fmt.Errorf("memory name %q is empty after normalization", name)
	}
	return filepath.Join(s.dir, slug+".md"), nil
}

// Save writes (or overwrites) the fact named name with the given one-line description and
// body, then regenerates the index.
func (s *Store) Save(name, description, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	fact := Fact{Name: name, Description: description, Modified: s.stamp()}
	if existing, rerr := os.ReadFile(p); rerr == nil {
		old := parseFact(existing)
		if old.Name != "" && old.Name != name {
			return fmt.Errorf(
				"memory name %q collides with existing memory %q (both map to %s); choose a more distinct name",
				name, old.Name, filepath.Base(p))
		}
		fact.Used, fact.Uses = old.Used, old.Uses
	}
	if err := writeFact(p, fact, body); err != nil {
		return err
	}
	return s.regenerate()
}

// Read returns the full on-disk content (frontmatter + body) of the named fact and records
// the use (used timestamp + use count) in its frontmatter, best-effort: a failed stamp is
// logged, not fatal. A file whose frontmatter is missing, malformed, or unnamed is returned
// as-is rather than risk rewriting it.
func (s *Store) Read(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no memory named %q", name)
		}
		return "", err
	}
	fact, body, ok := splitFact(data)
	if !ok || fact.Name == "" {
		return string(data), nil
	}
	fact.Used = s.stamp()
	fact.Uses++
	if fact.Modified == "" {
		// A pre-metadata fact: its mtime is the only age signal, and the rewrite below
		// would destroy it.
		fact.Modified = mtimeStamp(p)
	}
	// The stamp is telemetry, not the payload: a full or read-only disk must not make
	// memory unreadable.
	if err := writeFact(p, fact, body); err != nil {
		slog.Warn("memory usage stamp failed", "fact", fact.Name, "err", err)
		return string(data), nil
	}
	return factContent(fact, body)
}

// Inspect returns a fact's content without recording a use, looking in the active set first
// and then the archive. The daily review inspects facts this way so judging a fact does not
// refresh the very usage signal the judgment relies on.
func (s *Store) Inspect(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		data, err = os.ReadFile(filepath.Join(s.dir, archiveDir, filepath.Base(p)))
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no memory named %q, active or archived", name)
		}
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Delete removes the named fact and regenerates the index.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no memory named %q", name)
		}
		return err
	}
	return s.regenerate()
}

// Archive moves the named fact out of the active set (and the index) into the archive
// subdirectory, where it stays recoverable via Restore.
func (s *Store) Archive(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no memory named %q", name)
		}
		return err
	}
	adir := filepath.Join(s.dir, archiveDir)
	dst := filepath.Join(adir, filepath.Base(p))
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("an archived memory named %q already exists and archiving would destroy it; "+
			"delete the active fact instead, or save it under a new name first", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(adir, 0o755); err != nil {
		return err
	}
	if err := os.Rename(p, dst); err != nil {
		return err
	}
	return s.regenerate()
}

// Restore moves an archived fact back into the active set.
func (s *Store) Restore(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("an active memory named %q already exists; restoring would overwrite it", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	src := filepath.Join(s.dir, archiveDir, filepath.Base(p))
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no archived memory named %q", name)
		}
		return err
	}
	if err := os.Rename(src, p); err != nil {
		return err
	}
	return s.regenerate()
}

// List returns all facts (excluding the generated index), sorted by name.
func (s *Store) List() ([]Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

// list reads the directory and parses each fact's frontmatter. Caller holds s.mu.
func (s *Store) list() ([]Fact, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var facts []Fact
	for _, e := range entries {
		if e.IsDir() || e.Name() == indexFile || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		f := parseFact(data)
		if f.Name == "" {
			f.Name = strings.TrimSuffix(e.Name(), ".md")
		}
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Name < facts[j].Name })
	return facts, nil
}

// Index returns the injectable index block (one line per fact), or "" when memory is empty.
func (s *Store) Index() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.list()
	if err != nil || len(facts) == 0 {
		return "", err
	}
	return formatIndex(facts), nil
}

// Audit returns a staleness overview: one line per active fact (age since modified, last
// use, use count) plus the archived fact names. Age falls back to file mtime for facts
// saved before metadata existed.
func (s *Store) Audit() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.list()
	if err != nil {
		return "", err
	}
	now := s.now()
	var b strings.Builder
	b.WriteString("# Memory audit\n")
	if len(facts) == 0 {
		b.WriteString("(no facts)\n")
	}
	for _, f := range facts {
		modified := f.Modified
		if modified == "" {
			if p, perr := s.path(f.Name); perr == nil {
				modified = mtimeStamp(p)
			}
		}
		used := "never"
		if f.Used != "" {
			used = age(now, f.Used)
		}
		fmt.Fprintf(&b, "- %s: modified %s, last used %s, uses %d\n", f.Name, age(now, modified), used, f.Uses)
	}
	archived, err := s.archivedNames()
	if err != nil {
		return "", err
	}
	if len(archived) == 0 {
		b.WriteString("Archived: (none)")
	} else {
		b.WriteString("Archived: " + strings.Join(archived, ", "))
	}
	return b.String(), nil
}

// archivedNames lists archived fact slugs. Caller holds s.mu.
func (s *Store) archivedNames() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, archiveDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names, nil
}

func age(now time.Time, stamp string) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "unknown"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// regenerate rewrites MEMORY.md from the current facts. Caller holds s.mu.
func (s *Store) regenerate() error {
	facts, err := s.list()
	if err != nil {
		return err
	}
	if len(facts) == 0 {
		err := os.Remove(filepath.Join(s.dir, indexFile))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(filepath.Join(s.dir, indexFile), []byte(formatIndex(facts)+"\n"), 0o644)
}

func formatIndex(facts []Fact) string {
	var b strings.Builder
	b.WriteString("# Memory index\n")
	for _, f := range facts {
		b.WriteString("- ")
		b.WriteString(f.Name)
		if f.Description != "" {
			b.WriteString(" - ")
			b.WriteString(f.Description)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseFact(data []byte) Fact {
	f, _, _ := splitFact(data)
	return f
}

// splitFact separates a fact file into parsed frontmatter and raw body; ok is false when
// the frontmatter is missing or malformed.
func splitFact(data []byte) (Fact, string, bool) {
	m := frontmatterRe.FindSubmatch(data)
	if m == nil {
		return Fact{}, "", false
	}
	var f Fact
	if err := yaml.Unmarshal(m[1], &f); err != nil {
		return Fact{}, "", false
	}
	return f, string(m[2]), true
}

func factContent(f Fact, body string) (string, error) {
	fm, err := yaml.Marshal(f)
	if err != nil {
		return "", err
	}
	return "---\n" + string(fm) + "---\n" + strings.TrimRight(body, "\n") + "\n", nil
}

// writeFact writes via a same-directory temp file and rename, so a crash mid-write can
// never leave a truncated fact - reads rewrite facts too, so this path runs constantly.
func writeFact(path string, f Fact, body string) error {
	content, err := factContent(f, body)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write([]byte(content)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func mtimeStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().UTC().Format(time.RFC3339)
}
