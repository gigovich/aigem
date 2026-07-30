// Package tools implements the agent's tool set, sandboxed under a root directory.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sahilm/fuzzy"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/pathgrant"
)

// Tool is a single capability the model can invoke.
type Tool interface {
	Name() string
	Description() string
	// Schema returns the JSON Schema for the tool's arguments object.
	Schema() json.RawMessage
	// NeedsConfirm reports whether the TUI must ask the user before running.
	NeedsConfirm() bool
	// Run executes the tool with raw JSON arguments and returns a textual result.
	// ctx is cancelled when the user interrupts the turn.
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// FileChange describes a file written or edited by a tool, reported to callers
// that track session artifacts. Old is the content before the change (empty when
// the file was created); New is the content after.
type FileChange struct {
	Path    string // resolved absolute path
	Old     string
	New     string
	Created bool
}

// Registry holds the available tools and the sandbox root.
type Registry struct {
	root         string
	tools        map[string]Tool
	order        []string
	inContext    map[string]bool  // canonical paths whose contents are already in context
	mcp          map[string]bool  // tools sourced from MCP servers (main agent only)
	onFileChange func(FileChange) // optional artifact hook, fired by write/edit tools

	// Escaping the sandbox is off unless a front-end opts in: an unattended bot
	// leaves both unset and keeps the hard refusal. Tool instances hold the
	// registry they were built from, so a Subset shares these.
	pathMu       sync.RWMutex
	pathApprover PathApprover // asks the user; nil refuses outright
	pathGrants   bool         // consult the persisted per-project grants
}

// PathIntent describes why a tool wants a path outside the working directory.
type PathIntent struct {
	Tool  string
	Write bool
}

// PathDecision is the answer to an out-of-root path request.
type PathDecision int

const (
	PathDeny PathDecision = iota
	// PathAllowOnce permits this one call and is not remembered.
	PathAllowOnce
	// PathAllowDir permits the path's directory and everything under it, and is
	// persisted for this project. Only offered for reads.
	PathAllowDir
)

// PathApprover asks the user about a path outside the working directory. It is
// called from the tool's goroutine and may block while the user decides.
type PathApprover func(path string, intent PathIntent) PathDecision

// SetPathApprover installs the callback consulted when a tool asks for a path
// outside the sandbox root. Without one, such a path is refused as before.
func (r *Registry) SetPathApprover(fn PathApprover) {
	r.pathMu.Lock()
	r.pathApprover = fn
	r.pathMu.Unlock()
}

// SetPathGrants enables the persisted per-project directory grants, so a
// directory the user already approved is read without asking again. Off by
// default: a bot must not inherit a grant a human made for the same directory.
func (r *Registry) SetPathGrants(enabled bool) {
	r.pathMu.Lock()
	r.pathGrants = enabled
	r.pathMu.Unlock()
}

func (r *Registry) pathPolicy() (PathApprover, bool) {
	r.pathMu.RLock()
	defer r.pathMu.RUnlock()
	return r.pathApprover, r.pathGrants
}

func NewRegistry(root string) (*Registry, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Canonicalize the root so symlink containment checks compare like with like.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	r := &Registry{root: abs, tools: map[string]Tool{}, inContext: map[string]bool{}, mcp: map[string]bool{}}
	r.add(&readFile{r})
	r.add(&writeFile{r})
	r.add(&editFile{r})
	r.add(&listDir{r})
	r.add(&bashTool{r})
	r.add(&grepTool{r})
	r.add(&fuzzyFind{r})
	return r, nil
}

// Root returns the registry's sandbox root, an absolute, symlink-resolved path.
func (r *Registry) Root() string { return r.root }

// OnFileChange registers a hook fired after write_file or edit_file mutates a
// file, letting the front-end track per-session artifacts. Subsets inherit it,
// so subagent edits are reported too.
func (r *Registry) OnFileChange(fn func(FileChange)) { r.onFileChange = fn }

func (r *Registry) reportFileChange(c FileChange) {
	if r.onFileChange != nil {
		r.onFileChange(c)
	}
}

func (r *Registry) add(t Tool) {
	r.tools[t.Name()] = t
	r.order = append(r.order, t.Name())
}

// Register adds (or replaces) a tool after construction, e.g. the delegation
// tool wired up once its dependencies exist.
func (r *Registry) Register(t Tool) {
	if _, exists := r.tools[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Unregister removes a dynamically configured tool from the registry.
func (r *Registry) Unregister(name string) {
	if _, exists := r.tools[name]; !exists {
		return
	}
	delete(r.tools, name)
	delete(r.mcp, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

// RegisterMCP adds a tool sourced from an MCP server. Such tools reach the main
// agent only: Subset drops them, so subagents (and forked skills) keep the
// built-in toolset.
func (r *Registry) RegisterMCP(t Tool) {
	r.Register(t)
	r.mcp[t.Name()] = true
}

// Subset returns a registry exposing only the named tools, sharing this
// registry's sandbox root. Unknown names and MCP-sourced tools are skipped.
func (r *Registry) Subset(names []string) *Registry {
	sub := &Registry{root: r.root, tools: map[string]Tool{}, inContext: r.inContext,
		mcp: map[string]bool{}, onFileChange: r.onFileChange}
	for _, n := range names {
		if r.mcp[n] {
			continue
		}
		if t, ok := r.tools[n]; ok {
			sub.add(t)
		}
	}
	return sub
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// MarkInContext records files whose full contents are already in the model's
// context (e.g. injected project instructions), so read_file returns a short
// note instead of re-emitting them. Paths are canonicalized to match resolve.
// Subsets share these tool instances, so subagents honor it too.
func (r *Registry) MarkInContext(paths []string) {
	for _, p := range paths {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			r.inContext[real] = true
		}
	}
}

// inContextNote returns a stand-in message if canonical path p is already in
// context, else "".
func (r *Registry) inContextNote(p, display string) string {
	if r.inContext[p] {
		return fmt.Sprintf("(%s is a project instruction file already included in your context in "+
			"full - see the project conventions above. Not re-read.)", display)
	}
	return ""
}

// Names returns the registered tool names in order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Definitions returns the tool list in OpenAI format for a chat request.
func (r *Registry) Definitions() []llm.Tool {
	defs := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, llm.Tool{
			Type: "function",
			Function: llm.ToolDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return defs
}

// resolveFor maps a model-supplied path into the sandbox. A path outside it is
// allowed only when the user says so: silently for a directory already granted
// to this project, otherwise by asking the front-end. With neither a grant nor
// an approver the refusal is the same as it has always been.
func (r *Registry) resolveFor(p string, intent PathIntent) (string, error) {
	clean, inside, err := r.locate(p)
	if err != nil {
		return "", err
	}
	if inside {
		return clean, nil
	}
	approver, grants := r.pathPolicy()
	// Grants cover reads only, so a write outside the root always asks.
	if grants && !intent.Write {
		ok, err := pathgrant.Allowed(r.root, clean)
		if err != nil {
			return "", fmt.Errorf("check path grants: %w", err)
		}
		if ok {
			return clean, nil
		}
	}
	if approver == nil {
		return "", outsideRootErr(p, r.root)
	}
	switch approver(clean, intent) {
	case PathAllowOnce:
		return clean, nil
	case PathAllowDir:
		if grants && !intent.Write {
			// Grant the folder the confirmation box actually named, and only for
			// a path that really exists. Taking the parent unconditionally would
			// turn "always allow /srv/data" into a grant over /srv and every
			// sibling; doing it for a path the model merely invented would let it
			// farm grants over directories the box never named, on calls that go
			// on to fail anyway.
			if dir, ok := grantDir(clean); ok {
				if err := pathgrant.Add(r.root, dir); err != nil {
					return "", fmt.Errorf("record path grant: %w", err)
				}
			}
		}
		return clean, nil
	}
	return "", fmt.Errorf("the user denied access to %q, which is outside the working directory %q - "+
		"do not ask for it again this turn; work with paths inside the working directory, or "+
		"explain what you needed it for", p, r.root)
}

// locate canonicalizes a model-supplied path and reports whether it lands
// inside the sandbox root.
func (r *Registry) locate(p string) (clean string, inside bool, err error) {
	if p == "" {
		p = "."
	}
	joined := p
	if !filepath.IsAbs(p) {
		joined = filepath.Join(r.root, p)
	}
	clean, err = resolveDeepest(filepath.Clean(joined))
	if err != nil {
		return "", false, err
	}
	rel, err := filepath.Rel(r.root, clean)
	if err != nil {
		return "", false, err
	}
	escapes := rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	return clean, !escapes, nil
}

// maxSymlinkHops bounds manual link following, mirroring the kernel's ELOOP
// limit so a cycle of dangling links cannot spin.
const maxSymlinkHops = 40

// resolveDeepest resolves symlinks in the deepest *existing* ancestor of p and
// rejoins the components that do not exist yet.
//
// Resolving only p and its immediate parent is not enough: as soon as two or
// more trailing components are missing, both lookups fail with ENOENT, the path
// stays unresolved, and a purely lexical containment check then accepts a path
// that actually lands outside the root. write_file's MkdirAll would create the
// missing directories straight through the link. A repository can ship such a
// symlink, so this has to hold for paths that do not exist yet.
func resolveDeepest(p string) (string, error) { return resolveHops(p, 0) }

func resolveHops(p string, hops int) (string, error) {
	if hops > maxSymlinkHops {
		return "", fmt.Errorf("too many levels of symbolic links resolving %q", p)
	}
	var missing []string
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return rejoin(real, missing), nil
		}
		// EvalSymlinks reports ENOENT both for a component that does not exist
		// and for one that IS a symlink whose target does not exist. Lstat tells
		// them apart. A dangling link has to be followed by hand: treating it as
		// a plain missing name would rejoin it onto the resolved parent and hand
		// back a path that sits lexically inside the root, while the kernel
		// follows the link outside it the moment write_file creates the file.
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(cur)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			resolved, err := resolveHops(filepath.Clean(target), hops+1)
			if err != nil {
				return "", err
			}
			return rejoin(resolved, missing), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil // walked to the filesystem root without finding anything
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// grantDir returns the directory an "always allow this folder" approval should
// record for an approved path, and whether anything should be recorded at all.
// A path that does not exist grants nothing: there is no folder the user can be
// said to have been shown.
func grantDir(clean string) (string, bool) {
	fi, err := os.Stat(clean)
	switch {
	case err != nil:
		return "", false
	case fi.IsDir():
		return clean, true
	default:
		return filepath.Dir(clean), true
	}
}

// rejoin appends the not-yet-existing components, outermost first, back onto a
// resolved base.
func rejoin(base string, missing []string) string {
	for i := len(missing) - 1; i >= 0; i-- {
		base = filepath.Join(base, missing[i])
	}
	return base
}

func outsideRootErr(p, root string) error {
	return fmt.Errorf(
		"path %q is outside the working directory %q; only paths inside it are allowed - "+
			"use a path relative to the working directory", p, root)
}

// fsError turns a filesystem error into an actionable message the model can act on.
func fsError(action, path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("cannot %s %q: no such file or directory - "+
			"use list_dir or fuzzy_find to locate the correct path", action, path)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("cannot %s %q: permission denied", action, path)
	default:
		return fmt.Errorf("cannot %s %q: %w", action, path, err)
	}
}

// fsLookupError is fsError for path lookups (read/list/edit): on a missing path
// it appends the nearest existing matches so the model can correct itself in one
// step instead of falling back to fuzzy_find. wantDir selects directory matches.
func (r *Registry) fsLookupError(action, path string, wantDir bool, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return fsError(action, path, err)
	}
	base := fmt.Sprintf("cannot %s %q: no such file or directory", action, path)
	if s := r.suggestPaths(path, wantDir, 3); len(s) > 0 {
		return fmt.Errorf("%s - did you mean: %s ? otherwise use list_dir or fuzzy_find",
			base, strings.Join(s, ", "))
	}
	return fmt.Errorf("%s - use list_dir or fuzzy_find to locate the correct path", base)
}

// suggestPaths returns up to n existing paths (files, or directories when
// wantDir) whose path best fuzzy-matches the missing query.
func (r *Registry) suggestPaths(query string, wantDir bool, n int) []string {
	const maxScan = 50000
	var cands []string
	_ = filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() != wantDir {
			return nil
		}
		rel, err := filepath.Rel(r.root, path)
		if err != nil || rel == "." {
			return nil
		}
		cands = append(cands, rel)
		if len(cands) >= maxScan {
			return filepath.SkipAll
		}
		return nil
	})
	matches := fuzzy.Find(query, cands)
	out := make([]string, 0, n)
	for i, m := range matches {
		if i >= n {
			break
		}
		out = append(out, m.Str)
	}
	return out
}
