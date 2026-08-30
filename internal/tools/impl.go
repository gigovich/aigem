package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sahilm/fuzzy"
)

const maxReadBytes = 256 * 1024

// bashWaitDelay bounds how long a finished or cancelled command is waited on
// when something it started in the background still holds the output pipe. The
// same two seconds internal/hooks uses, and a variable so a test can shrink it.
var bashWaitDelay = 2 * time.Second

const (
	// maxGrepMatches caps how many matching lines grep returns.
	maxGrepMatches = 200
	// maxGrepLineLen caps a single matched line. Minified bundles and source maps
	// pack 100KB+ onto one line; without this a handful of matches blow past the
	// model's context window.
	maxGrepLineLen = 512
	// maxGrepOutput caps grep's total output, a second guard against many medium lines.
	maxGrepOutput = 64 * 1024
)

// vendorDirs are non-hidden directories grep and fuzzy_find never descend into:
// dependency and build output that bloats results with vendored or generated
// code. Hidden (dot) directories are skipped separately - see skipSearchDir.
var vendorDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "__pycache__": true,
}

// skipSearchDir reports whether a directory should be skipped while searching
// file contents or paths. Like ripgrep, hidden (dot) directories are skipped by
// default: in real trees they hold VCS metadata (.git), virtualenvs (.venv), and
// agent scratch directories whose notes and progress logs otherwise
// dominate a TODO/FIXME search and pull the model toward summarizing prose
// instead of reading code. The caller must exempt the search root so a search
// rooted inside a dot-directory still runs.
func skipSearchDir(name string) bool {
	return (len(name) > 1 && name[0] == '.') || vendorDirs[name]
}

// ---- read_file ----

type readFile struct{ r *Registry }

func (t *readFile) Name() string       { return "read_file" }
func (t *readFile) NeedsConfirm() bool { return false }
func (t *readFile) Description() string {
	return "Read a UTF-8 text file. Each line is prefixed with its line number and a '| ' marker so you " +
		"can cite exact path:line; that 'NNN| ' prefix is NOT part of the file, so do not include it in " +
		"edit_file old_string (copy only the text after the '| ')."
}

func (t *readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"path":{"type":"string","description":"File path relative to the working directory."}},
		"required":["path"]
	}`)
}

func (t *readFile) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	p, err := t.r.resolveFor(a.Path, PathIntent{Tool: "read_file"})
	if err != nil {
		return "", err
	}
	if note := t.r.inContextNote(p, a.Path); note != "" {
		return note, nil
	}
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return "", fmt.Errorf("cannot read %q: it is a directory - use list_dir instead", a.Path)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", t.r.fsLookupError("read", a.Path, false, err)
	}
	if len(data) > maxReadBytes {
		return numberLines(string(data[:maxReadBytes])) + "\n... [truncated]", nil
	}
	if len(data) == 0 {
		return "(empty file)", nil
	}
	return numberLines(string(data)), nil
}

// gutterRe matches the "NNN| " line-number gutter that read_file prepends, so
// edit_file can tolerate old_string/new_string copied with the gutter still
// attached (a common model mistake now that read_file numbers its output). The
// separator is "| " rather than a tab so it cannot blur into the tab indentation
// of the code, which made the model miscount leading tabs and break edits.
var gutterRe = regexp.MustCompile(`^ *\d+\| `)

// numberLines prefixes each line with a right-aligned line number and a "| "
// marker, the way editors and other coding agents present files. It helps the
// model cite accurate path:line and reason about locations; edit_file strips the
// gutter back off when matching (see stripGutter). A trailing newline is preserved.
func numberLines(s string) string {
	lines := strings.Split(s, "\n")
	trailingNL := len(lines) > 0 && lines[len(lines)-1] == ""
	if trailingNL {
		lines = lines[:len(lines)-1]
	}
	for i, ln := range lines {
		lines[i] = fmt.Sprintf("%5d| %s", i+1, ln)
	}
	out := strings.Join(lines, "\n")
	if trailingNL {
		out += "\n"
	}
	return out
}

// stripGutter removes a leading line-number gutter from every line, undoing
// numberLines so an old_string/new_string the model copied verbatim from
// read_file output still matches the real (unnumbered) file content.
func stripGutter(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = gutterRe.ReplaceAllString(ln, "")
	}
	return strings.Join(lines, "\n")
}

// ---- write_file ----

type writeFile struct{ r *Registry }

func (t *writeFile) Name() string       { return "write_file" }
func (t *writeFile) NeedsConfirm() bool { return true }
func (t *writeFile) Description() string {
	return "Create a new file, or completely replace an existing file's contents. " +
		"The content REPLACES the whole file, so you must pass the entire desired file content. " +
		"To change part of an existing file, use edit_file instead."
}

func (t *writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path relative to the working directory."},
			"content":{"type":"string","description":"The COMPLETE file content. This overwrites the entire file."}
		},
		"required":["path","content"]
	}`)
}

func (t *writeFile) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	p, err := t.r.resolveFor(a.Path, PathIntent{Tool: "write_file", Write: true})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fsError("create parent directory for", a.Path, err)
	}
	old, existed := "", true
	if b, rerr := os.ReadFile(p); rerr == nil {
		old = string(b)
	} else if os.IsNotExist(rerr) {
		existed = false
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", fsError("write", a.Path, err)
	}
	t.r.reportFileChange(FileChange{Path: p, Old: old, New: a.Content, Created: !existed})
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
}

// ---- edit_file ----

type editFile struct{ r *Registry }

func (t *editFile) Name() string       { return "edit_file" }
func (t *editFile) NeedsConfirm() bool { return true }
func (t *editFile) Description() string {
	return "Edit an existing file by replacing an exact text fragment. Prefer this over write_file " +
		"for changes to existing files. Read the file first and copy old_string verbatim (including " +
		"indentation and newlines). old_string must be unique unless replace_all is true."
}

func (t *editFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path relative to the working directory."},
			"old_string":{"type":"string","description":"Exact text to find, copied verbatim from the file."},
			"new_string":{"type":"string","description":"Text to replace it with."},
			"replace_all":{"type":"boolean","description":"Replace every occurrence instead of requiring a unique match."}
		},
		"required":["path","old_string","new_string"]
	}`)
}

func (t *editFile) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.OldString == "" {
		return "", fmt.Errorf("old_string must not be empty; use write_file to create a new file")
	}
	if a.OldString == a.NewString {
		return "", fmt.Errorf("old_string and new_string are identical; nothing to change")
	}
	p, err := t.r.resolveFor(a.Path, PathIntent{Tool: "edit_file", Write: true})
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", t.r.fsLookupError("edit", a.Path, false, err)
	}
	content := string(data)
	oldStr, newStr := a.OldString, a.NewString
	count := strings.Count(content, oldStr)
	// Fallback: if the exact text is not found but old_string carries read_file's
	// line-number gutter, strip the gutter from both strings and retry. Exact
	// matching is tried first, so unnumbered edits are unaffected.
	if count == 0 {
		if stripped := stripGutter(oldStr); stripped != oldStr {
			if c := strings.Count(content, stripped); c > 0 {
				oldStr, newStr, count = stripped, stripGutter(newStr), c
			}
		}
	}
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string not found in %q; read the file and copy the exact text "+
			"(including whitespace) to replace", a.Path)
	case count > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("old_string occurs %d times in %q; add surrounding context to make it "+
			"unique, or set replace_all=true", count, a.Path)
	}
	n := 1
	if a.ReplaceAll {
		n = -1
	}
	updated := strings.Replace(content, oldStr, newStr, n)
	if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
		return "", fsError("write", a.Path, err)
	}
	t.r.reportFileChange(FileChange{Path: p, Old: content, New: updated})
	return fmt.Sprintf("replaced %d occurrence(s) in %s", count, a.Path), nil
}

// ---- list_dir ----

type listDir struct{ r *Registry }

func (t *listDir) Name() string        { return "list_dir" }
func (t *listDir) NeedsConfirm() bool  { return false }
func (t *listDir) Description() string { return "List the entries of a directory." }

func (t *listDir) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"path":{"type":"string","description":"Directory path relative to the working directory. Defaults to '.'"}},
		"required":[]
	}`)
}

func (t *listDir) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &a)
	p, err := t.r.resolveFor(a.Path, PathIntent{Tool: "list_dir"})
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", t.r.fsLookupError("list", a.Path, true, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(&sb, "%s/\n", e.Name())
		} else {
			fmt.Fprintf(&sb, "%s\n", e.Name())
		}
	}
	if sb.Len() == 0 {
		return "(empty)", nil
	}
	return sb.String(), nil
}

// ---- bash ----

// bashTool runs arbitrary shell commands. cmd.Dir scopes the working directory
// but is NOT a real jail: a command can still reach the whole filesystem and
// network. The TUI confirmation gate (NeedsConfirm) is the only safeguard.
type bashTool struct{ r *Registry }

// bashHardDeny lists commands blocked outright (never run, even unattended):
// catastrophic, irreversible operations. They are matched as the leading command
// of a pipeline segment, so a binary name that only appears inside an argument or
// token - e.g. "dd" inside an API key like "...634dda3c" - is not falsely blocked.
var bashHardDeny = map[string]bool{
	"dd": true, "mkfs": true, "fdisk": true, "mkdev": true, "shred": true,
}

// bashHardDenyPatterns are specific catastrophic argument forms blocked outright,
// matched as substrings of the lowercased command.
var bashHardDenyPatterns = []string{"rm -rf /", "rm -rf ."}

// deniedBashPattern returns the forbidden pattern matched in cmd, or "" if none.
// It reuses the segment/leading-token parsing from destructive.go so only an
// actual command head trips bashHardDeny, not a substring of an argument.
func deniedBashPattern(cmd string) string {
	lc := strings.ToLower(cmd)
	for _, p := range bashHardDenyPatterns {
		if strings.Contains(lc, p) {
			return p
		}
	}
	for _, seg := range segmentSplit.Split(cmd, -1) {
		head := leadingToken(seg)
		if i := strings.LastIndexByte(head, '/'); i >= 0 {
			head = head[i+1:]
		}
		h := strings.ToLower(head)
		// Match "mkfs.ext4" and similar variants by their pre-dot base too.
		if base, _, ok := strings.Cut(h, "."); ok && bashHardDeny[base] {
			return base
		}
		if bashHardDeny[h] {
			return h
		}
	}
	return ""
}

func (t *bashTool) Name() string       { return "bash" }
func (t *bashTool) NeedsConfirm() bool { return true }
func (t *bashTool) Description() string {
	return "Run a shell command in the working directory and return combined stdout and stderr."
}

func (t *bashTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"cmd":{"type":"string","description":"The shell command to execute."}},
		"required":["cmd"]
	}`)
}

func (t *bashTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}

	if p := deniedBashPattern(a.Cmd); p != "" {
		return "", fmt.Errorf("command denied: contains forbidden pattern %q", p)
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", a.Cmd)
	cmd.Dir = t.r.root
	configureProcessGroup(cmd)
	// Without these two, cancelling kills bash and then CombinedOutput waits for
	// whatever the command backgrounded, because the orphan still holds the
	// output pipe. Measured at 30s for a `sleep 30 &`, and unbounded for a dev
	// server - all of it inside a turn the caller believes it has cancelled, and
	// which closing the session now waits for.
	cmd.WaitDelay = bashWaitDelay
	out, err := cmd.CombinedOutput()
	res := string(out)
	switch {
	case errors.Is(err, exec.ErrWaitDelay):
		// The command itself finished; what ran out was the wait for a child it
		// left holding the output pipe. Reporting that as an exit error would
		// tell the model a command failed when it did not - and starting a
		// server in the background is a thing people ask for.
		res += "\n[the command finished; something it started in the background is " +
			"still running and still holding its output]"
	case err != nil:
		res += fmt.Sprintf("\n[exit error: %v]", err)
	}
	if res == "" {
		res = "(no output)"
	}
	return res, nil
}

// ---- grep ----

type grepTool struct{ r *Registry }

func (t *grepTool) Name() string       { return "grep" }
func (t *grepTool) NeedsConfirm() bool { return false }
func (t *grepTool) Description() string {
	return "Search file contents with a regular expression. Returns matching file:line:text."
}

func (t *grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"pattern":{"type":"string","description":"RE2 regular expression."},
			"path":{"type":"string","description":"File or directory to search. Defaults to '.'"}
		},
		"required":["pattern"]
	}`)
}

func (t *grepTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	root, err := t.r.resolveFor(a.Path, PathIntent{Tool: "grep"})
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	count, clipped := 0, 0
	capped := false
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipSearchDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > maxReadBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || isBinary(data) {
			return nil
		}
		rel, _ := filepath.Rel(t.r.root, path)
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			line = strings.TrimSpace(line)
			if len(line) > maxGrepLineLen {
				line = strings.ToValidUTF8(line[:maxGrepLineLen], "") + " ... [line truncated]"
				clipped++
			}
			fmt.Fprintf(&sb, "%s:%d:%s\n", rel, i+1, line)
			count++
			if count >= maxGrepMatches || sb.Len() >= maxGrepOutput {
				capped = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if count == 0 {
		return "(no matches)", nil
	}
	if capped {
		sb.WriteString("... [output limit reached - narrow the pattern or path]\n")
	}
	if clipped > 0 {
		fmt.Fprintf(&sb, "... [%d long line(s) truncated]\n", clipped)
	}
	return sb.String(), nil
}

// ---- fuzzy_find ----

type fuzzyFind struct{ r *Registry }

func (t *fuzzyFind) Name() string       { return "fuzzy_find" }
func (t *fuzzyFind) NeedsConfirm() bool { return false }
func (t *fuzzyFind) Description() string {
	return "Find files by fuzzy-matching their paths against a query."
}

func (t *fuzzyFind) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"query":{"type":"string","description":"Fuzzy query matched against file paths."}},
		"required":["query"]
	}`)
}

func (t *fuzzyFind) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	const maxFiles = 50000
	var paths []string
	walkErr := filepath.WalkDir(t.r.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != t.r.root && skipSearchDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(t.r.root, path)
		paths = append(paths, rel)
		if len(paths) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	sort.Strings(paths)

	matches := fuzzy.Find(a.Query, paths)
	const maxResults = 30
	var sb strings.Builder
	for i, m := range matches {
		if i >= maxResults {
			break
		}
		sb.WriteString(m.Str)
		sb.WriteByte('\n')
	}
	if sb.Len() == 0 {
		return "(no matches)", nil
	}
	return sb.String(), nil
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
