// Package skill discovers and renders Claude-compatible Agent Skills.
package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gigovich/aigem/internal/config"
	projecttrust "github.com/gigovich/aigem/internal/trust"
)

// Skill is a discovered SKILL.md capability. The whole file is parsed at discovery;
// the markdown body is sent to the model only on Render (progressive disclosure).
type Skill struct {
	Name            string
	Description     string
	WhenToUse       string
	ArgHint         string
	Args            []string // named positional argument names for $name substitution
	AllowedTools    []string
	DisallowedTools []string
	Model           string
	Effort          string
	Context         string // "fork" to run in an isolated subagent
	Agent           string // subagent type when Context == "fork"
	Paths           []string
	Shell           string

	DisableModelInvocation bool
	UserInvocable          bool // default true
	Hooks                  map[string]any

	Dir          string // skill directory (resolves ${CLAUDE_SKILL_DIR}); empty for builtins
	Path         string // SKILL.md path
	ProjectLocal bool   // discovered from the current project's skill roots
	Builtin      bool   // embedded in the binary rather than discovered on disk

	body   string // parsed at discovery, sent to the model only on Render
	pathRe []*regexp.Regexp
}

// ModelInvocable reports whether the model may auto-invoke this skill.
func (s *Skill) ModelInvocable() bool { return !s.DisableModelInvocation }

// Conditional reports whether the skill activates only for matching paths and so
// is kept out of the always-on listing until a relevant file is touched.
func (s *Skill) Conditional() bool { return len(s.Paths) > 0 }

// Matches reports whether rel (a slash path) matches any of the skill's paths
// globs, by full path or basename. Supports *, ?, and ** wildcards.
func (s *Skill) Matches(rel string) bool {
	if len(s.Paths) == 0 {
		return false
	}
	if s.pathRe == nil {
		for _, g := range s.Paths {
			if re := globToRegexp(filepath.ToSlash(g)); re != nil {
				s.pathRe = append(s.pathRe, re)
			}
		}
	}
	rel = filepath.ToSlash(rel)
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	for _, re := range s.pathRe {
		if re.MatchString(rel) || re.MatchString(base) {
			return true
		}
	}
	return false
}

// globToRegexp compiles a glob (with **, *, ?) into an anchored regexp.
func globToRegexp(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '(', ')', '+', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// Body returns the raw markdown body (for previews); it is not substituted.
func (s *Skill) Body() string { return s.body }

// flexList accepts a YAML scalar ("a b, c") or sequence as a string slice.
type flexList []string

func (f *flexList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*f = splitList(node.Value)
	case yaml.SequenceNode:
		var s []string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*f = s
	}
	return nil
}

var listSep = regexp.MustCompile(`[,\s]+`)

func splitList(s string) []string {
	var out []string
	for _, p := range listSep.Split(strings.TrimSpace(s), -1) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

type frontmatter struct {
	Name                   string         `yaml:"name"`
	Description            string         `yaml:"description"`
	WhenToUse              string         `yaml:"when_to_use"`
	ArgHint                string         `yaml:"argument-hint"`
	Arguments              flexList       `yaml:"arguments"`
	AllowedTools           flexList       `yaml:"allowed-tools"`
	DisallowedTools        flexList       `yaml:"disallowed-tools"`
	Model                  string         `yaml:"model"`
	Effort                 string         `yaml:"effort"`
	Context                string         `yaml:"context"`
	Agent                  string         `yaml:"agent"`
	Paths                  flexList       `yaml:"paths"`
	Shell                  string         `yaml:"shell"`
	DisableModelInvocation bool           `yaml:"disable-model-invocation"`
	UserInvocable          *bool          `yaml:"user-invocable"`
	Hooks                  map[string]any `yaml:"hooks"`
}

// Registry holds discovered skills in priority order.
type Registry struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]*Skill
}

func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Skill, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

func (r *Registry) Get(name string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byName[name]
	return s, ok
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// Names returns the model-invocable skill names (for the tool enum).
func (r *Registry) ModelNames() []string {
	var out []string
	for _, s := range r.List() {
		if s.ModelInvocable() {
			out = append(out, s.Name)
		}
	}
	return out
}

// Conditional returns skills gated on paths globs (loaded only when a matching
// file is touched), so the harness can watch for activation.
func (r *Registry) Conditional() []*Skill {
	var out []*Skill
	for _, s := range r.List() {
		if s.Conditional() {
			out = append(out, s)
		}
	}
	return out
}

// Prompt returns the model-facing skill listing for the system prompt
// (model-invocable, non-conditional skills), or "" if there are none.
func (r *Registry) Prompt() string {
	var b strings.Builder
	for _, s := range r.List() {
		if s.ModelInvocable() && !s.Conditional() {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Listing())
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "# Skills\n\nYou can invoke a skill with the `skill` tool when a task matches one - each " +
		"provides ready-made instructions, so prefer it over improvising. Available:\n" +
		strings.TrimRight(b.String(), "\n")
}

// Discover scans every skill root under cwd and the user config, returning a
// registry plus non-fatal errors for skipped files. The first source to claim a
// name wins (aigem .skills shadows .claude/skills).
func Discover(cwd string) (*Registry, []error) {
	r := &Registry{byName: map[string]*Skill{}}
	var errs []error
	trusted, err := projectSkillsAllowed(cwd)
	if err != nil {
		errs = append(errs, err)
	}
	for _, root := range config.SkillRoots(cwd) {
		if root.Project && !trusted {
			continue
		}
		errs = append(errs, r.scanRoot(root.Dir, root.Project)...)
	}
	sort.Strings(r.order)
	return r, errs
}

// ApproveProject approves the current set of project-local skills. Any change
// to those skill files invalidates the approval.
func ApproveProject(cwd string) error {
	projectDir := config.ProjectDir(cwd)
	fingerprint, sources, err := projectSkillsFingerprint(cwd)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("project has no skills to approve")
	}
	return projecttrust.Approve(projectDir, projecttrust.CapabilitySkills, "project-skills", fingerprint, "user")
}

// PendingSkills describes project-local skills that Discover withheld because
// the project's current skill definitions carry no approval. Nothing about them
// reaches the model, so a front-end must surface them rather than let discovery
// come up empty for no visible reason.
// Both string fields are sanitized for display: they originate in an unapproved
// repo, so they are shown to the user but never trusted.
type PendingSkills struct {
	Dir         string
	Names       []string
	Invalidated bool // approved once, but the definitions changed since
}

// Pending reports the project-local skills awaiting a trust decision, or nil
// when the project defines none, they are already approved, or a previous
// decision denied them. Skipping the decision is not a denial, so a project the
// user passed over stays pending and is reported again next time.
func Pending(cwd string) (*PendingSkills, error) {
	status, sources, err := projectSkillStatus(cwd)
	if err != nil || len(sources) == 0 {
		return nil, err
	}
	if status.State != projecttrust.StatePending && status.State != projecttrust.StateInvalidated {
		return nil, nil
	}
	p := &PendingSkills{
		Dir:         DisplaySafe(config.ProjectDir(cwd)),
		Invalidated: status.State == projecttrust.StateInvalidated,
	}
	// Names come from the same sources the fingerprint hashed, so the list the
	// user approves cannot disagree with the set the approval covers - including
	// definitions that could not be read or parsed, which still get approved.
	for _, src := range sources {
		p.Names = append(p.Names, DisplaySafe(sourceName(src)))
	}
	sort.Strings(p.Names)
	return p, nil
}

// sourceName is the label for one fingerprinted definition: its declared name
// when that can be read, else the directory it sits in. It never returns "" -
// sanitizing away a name must not sanitize away the fact that a skill is there,
// or a hostile repo could get itself approved by presenting an empty list.
func sourceName(src skillSource) string {
	if src.Root {
		return src.Path + "/* (unreadable directory)"
	}
	dir := path.Base(path.Dir(src.Path))
	if src.Unreadable {
		return dir + " (unreadable)"
	}
	sk, err := parse(src.Content)
	if err != nil {
		return dir + " (unparsable)"
	}
	name := sk.Name
	if name == "" {
		name = dir
	}
	if strings.TrimSpace(DisplaySafe(name)) == "" {
		return dir + " (unprintable name)"
	}
	return name
}

// DisplaySafe strips the characters a string can use to control a terminal
// rather than fill it: C0 and C1 controls (the escape that opens an ANSI
// sequence, and the single-byte CSI), plus the bidi and zero-width formatting
// runes that reorder or hide text without moving the cursor. Skill names and
// paths come out of an unapproved repo and are the evidence a user weighs before
// approving it, so a crafted one must not be able to repaint, reorder or spoof
// the prompt asking about it.
func DisplaySafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		case r == 0x00ad, r == 0xfeff:
			return -1
		case r == 0x061c, r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e:
			return -1
		case r >= 0x2060 && r <= 0x206f:
			return -1
		}
		return r
	}, s)
}

func projectSkillsAllowed(cwd string) (bool, error) {
	status, sources, err := projectSkillStatus(cwd)
	if err != nil || len(sources) == 0 {
		return false, err
	}
	return status.State == projecttrust.StateAllowed, nil
}

// projectSkillStatus migrates any legacy whole-project approval, then evaluates
// the current project-local skill set. It returns the fingerprinted sources so
// callers can describe exactly what the decision covers; the status is
// meaningless when there are none.
func projectSkillStatus(cwd string) (projecttrust.Status, []skillSource, error) {
	fingerprint, sources, err := projectSkillsFingerprint(cwd)
	if err != nil {
		return projecttrust.Status{}, nil, err
	}
	projectDir := config.ProjectDir(cwd)
	var targets []projecttrust.CurrentTarget
	if len(sources) > 0 {
		targets = []projecttrust.CurrentTarget{{Target: "project-skills", Fingerprint: fingerprint}}
	}
	if err := projecttrust.MigrateLegacy(projectDir, projecttrust.CapabilitySkills, targets); err != nil {
		return projecttrust.Status{}, nil, fmt.Errorf("migrate legacy project skill trust: %w", err)
	}
	if len(sources) == 0 {
		return projecttrust.Status{}, nil, nil
	}
	status, err := projecttrust.Evaluate(projectDir, projecttrust.CapabilitySkills, "project-skills", fingerprint)
	if err != nil {
		return projecttrust.Status{}, nil, fmt.Errorf("evaluate project skill trust: %w", err)
	}
	return status, sources, nil
}

type skillSource struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Unreadable records a skill file that exists but could not be read, so it
	// still contributes to the fingerprint. The reason is deliberately not
	// recorded: it would embed an absolute path, and every reason means the same
	// thing here.
	Unreadable bool `json:"unreadable,omitempty"`
	// Root marks Path as a whole skill directory rather than one SKILL.md. Both
	// tags are omitempty, so a tree with neither fingerprints as it always did.
	Root bool `json:"root,omitempty"`
}

// OutOfScopeAncestors returns skill directories above the project root, which
// belong to no project and so are neither loaded nor offered for approval.
// Outside a git repo the project root is cwd, which can put a directory that
// used to be scanned out of reach; naming it beats having the skills disappear.
func OutOfScopeAncestors(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	root := config.ProjectDir(abs)
	home, _ := os.UserHomeDir()
	var out []string
	for d := filepath.Dir(root); ; d = filepath.Dir(d) {
		for _, name := range []string{".skills", filepath.Join(".claude", "skills")} {
			p := filepath.Join(d, name)
			// The user's own ~/.claude/skills is always loaded, so it is in scope.
			if home != "" && p == filepath.Join(home, ".claude", "skills") {
				continue
			}
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				out = append(out, p)
			}
		}
		if d == filepath.Dir(d) {
			return out
		}
	}
}

// projectSkillsFingerprint hashes every project-local SKILL.md. A file or root
// it cannot read is folded in as unreadable rather than failing the whole
// fingerprint: aborting here would withhold the project's skills AND suppress
// the prompt that explains why, which is the silence this gate exists to avoid.
// The fingerprint still changes if the file later becomes readable.
func projectSkillsFingerprint(cwd string) (string, []skillSource, error) {
	projectDir := config.ProjectDir(cwd)
	var sources []skillSource
	rel := func(path string) string {
		r, err := filepath.Rel(projectDir, path)
		if err != nil {
			return filepath.ToSlash(path)
		}
		return filepath.ToSlash(r)
	}
	for _, root := range config.SkillRoots(cwd) {
		if !root.Project {
			continue
		}
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			sources = append(sources, skillSource{Path: rel(root.Dir), Unreadable: true, Root: true})
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(root.Dir, entry.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				sources = append(sources, skillSource{Path: rel(path), Unreadable: true})
				continue
			}
			sources = append(sources, skillSource{Path: rel(path), Content: string(data)})
		}
	}
	if len(sources) == 0 {
		return "", nil, nil
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	fingerprint, err := projecttrust.Fingerprint(sources)
	if err != nil {
		return "", nil, fmt.Errorf("fingerprint project skills: %w", err)
	}
	return fingerprint, sources, nil
}

// DiscoverDir scans a single directory for skills (each a <name>/SKILL.md). It is used for a
// bot's self-authored skills, which live outside the standard skill roots.
func DiscoverDir(dir string) (*Registry, []error) {
	r := &Registry{byName: map[string]*Skill{}}
	errs := r.scanRoot(dir, false)
	sort.Strings(r.order)
	return r, errs
}

// DiscoverFS scans a single fs.FS (each skill a <name>/SKILL.md) into a fresh registry,
// marking every skill Builtin. Used for skills embedded in the binary; their bodies must be
// static since they have no on-disk directory.
func DiscoverFS(fsys fs.FS) (*Registry, []error) {
	r := &Registry{byName: map[string]*Skill{}}
	errs := r.scanFS(fsys, "", false)
	sort.Strings(r.order)
	return r, errs
}

// MergeMissing adds other's skills whose names r has not already claimed, preserving r's
// first-name-wins precedence, then re-sorts the listing order.
func (r *Registry) MergeMissing(other *Registry) {
	incoming := other.List()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range incoming {
		if _, taken := r.byName[s.Name]; taken {
			continue
		}
		r.byName[s.Name] = s
		r.order = append(r.order, s.Name)
	}
	sort.Strings(r.order)
}

// Replace swaps r's contents for other's without changing r's identity, so
// holders of the pointer - the skill tool, the system-prompt builder - observe
// the new set instead of having to be rebuilt around a fresh registry.
func (r *Registry) Replace(other *Registry) {
	var incoming []*Skill
	if other != nil {
		incoming = other.List()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = nil
	r.byName = map[string]*Skill{}
	for _, s := range incoming {
		r.order = append(r.order, s.Name)
		r.byName[s.Name] = s
	}
}

// Remove drops a skill from the registry by name; unknown names are a no-op.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; !ok {
		return
	}
	delete(r.byName, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// scanRoot adds every <root>/<name>/SKILL.md skill to r, returning non-fatal parse errors. The
// first source to claim a name wins.
func (r *Registry) scanRoot(root string, projectLocal bool) []error {
	if root == "" {
		// os.DirFS("") would resolve "." to the filesystem root; the old os.ReadDir("")
		// errored out, and that empty-registry behavior must hold.
		return nil
	}
	return r.scanFS(os.DirFS(root), root, projectLocal)
}

// scanFS adds every <name>/SKILL.md under fsys to r, returning non-fatal parse errors. The
// first source to claim a name wins. diskRoot is the on-disk path of fsys's root, or "" for
// embedded sources: those are marked Builtin and their Dir stays empty, so Render never runs
// dynamic injection for them and the body must not rely on ${CLAUDE_SKILL_DIR}.
func (r *Registry) scanFS(fsys fs.FS, diskRoot string, projectLocal bool) []error {
	builtin := diskRoot == ""
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name()+"/SKILL.md")
		if err != nil {
			continue // not a skill dir
		}
		path := "builtin:" + e.Name()
		var dir string
		if diskRoot != "" {
			dir = filepath.Join(diskRoot, e.Name())
			path = filepath.Join(dir, "SKILL.md")
		}
		sk, err := parse(string(data))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if sk.Name == "" {
			sk.Name = e.Name()
		}
		sk.Dir, sk.Path = dir, path
		sk.ProjectLocal = projectLocal
		sk.Builtin = builtin
		if _, taken := r.byName[sk.Name]; taken {
			continue // higher-priority source already claimed it
		}
		r.byName[sk.Name] = sk
		r.order = append(r.order, sk.Name)
	}
	return errs
}

var frontRe = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n?(.*)`)

func parse(s string) (*Skill, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	m := frontRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("missing YAML frontmatter")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(m[1]), &fm); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	desc := strings.TrimSpace(fm.Description)
	body := strings.TrimLeft(m[2], "\n")
	if desc == "" {
		desc = firstParagraph(body)
	}
	userInvocable := true
	if fm.UserInvocable != nil {
		userInvocable = *fm.UserInvocable
	}
	return &Skill{
		Name:                   fm.Name,
		Description:            desc,
		WhenToUse:              strings.TrimSpace(fm.WhenToUse),
		ArgHint:                fm.ArgHint,
		Args:                   fm.Arguments,
		AllowedTools:           fm.AllowedTools,
		DisallowedTools:        fm.DisallowedTools,
		Model:                  fm.Model,
		Effort:                 fm.Effort,
		Context:                fm.Context,
		Agent:                  fm.Agent,
		Paths:                  fm.Paths,
		Shell:                  fm.Shell,
		DisableModelInvocation: fm.DisableModelInvocation,
		UserInvocable:          userInvocable,
		Hooks:                  fm.Hooks,
		body:                   body,
	}, nil
}

func firstParagraph(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.Index(body, "\n\n"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body)
}

// Listing returns the description used in the model's skill listing, combining
// description and when_to_use within the shared character budget.
func (s *Skill) Listing() string {
	d := s.Description
	if s.WhenToUse != "" {
		d += " " + s.WhenToUse
	}
	const budget = 1536
	if len(d) > budget {
		d = d[:budget]
	}
	return strings.TrimSpace(d)
}

// RenderOpts carries session values for variable substitution.
type RenderOpts struct {
	SessionID string
	Effort    string
}

// Render returns the skill's body with dynamic context injection executed and
// argument/variable substitution applied. args is the raw user argument string.
func (s *Skill) Render(ctx context.Context, args string, opts RenderOpts) (string, error) {
	body := s.body
	if s.Dir != "" {
		// A skill without an on-disk directory (embedded builtin) must stay static:
		// running its shell blocks would exec in the process cwd, not the skill dir.
		body = s.injectDynamic(ctx, body)
	}
	body = s.substitute(body, args, opts)
	if !strings.Contains(s.body, "$ARGUMENTS") && strings.TrimSpace(args) != "" {
		body = strings.TrimRight(body, "\n") + "\n\nARGUMENTS: " + args
	}
	return body, nil
}

var (
	inlineCmd = regexp.MustCompile("!`([^`]*)`")
	blockCmd  = regexp.MustCompile("(?s)```!\\n(.*?)\\n```")
)

// injectDynamic runs `!`cmd“ (inline) and ```! blocks, replacing each with its
// output. Commands run in the skill directory with a timeout. Skill files are
// trusted (author-provided), like Claude.
func (s *Skill) injectDynamic(ctx context.Context, body string) string {
	run := func(cmd string) string {
		return s.runShell(ctx, cmd)
	}
	body = blockCmd.ReplaceAllStringFunc(body, func(m string) string {
		return run(blockCmd.FindStringSubmatch(m)[1])
	})
	body = inlineCmd.ReplaceAllStringFunc(body, func(m string) string {
		return run(inlineCmd.FindStringSubmatch(m)[1])
	})
	return body
}

func (s *Skill) runShell(ctx context.Context, command string) string {
	shell, flag := "bash", "-c"
	if s.Shell == "powershell" {
		shell, flag = "powershell", "-Command"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, shell, flag, command)
	c.Dir = s.Dir
	c.Env = append(os.Environ(), "CLAUDE_SKILL_DIR="+s.Dir)
	out, err := c.CombinedOutput()
	res := strings.TrimRight(string(out), "\n")
	if err != nil {
		res += fmt.Sprintf(" [error: %v]", err)
	}
	return res
}

// substitute applies $ARGUMENTS, $0..$n, named $name, and ${CLAUDE_*} variables.
func (s *Skill) substitute(body, args string, opts RenderOpts) string {
	fields := shellSplit(args)
	repl := map[string]string{
		"${CLAUDE_SKILL_DIR}":  s.Dir,
		"${CLAUDE_SESSION_ID}": opts.SessionID,
		"${CLAUDE_EFFORT}":     opts.Effort,
		"$ARGUMENTS":           args,
	}
	for i, f := range fields {
		repl["$"+strconv.Itoa(i)] = f
	}
	for i, name := range s.Args {
		v := ""
		if i < len(fields) {
			v = fields[i]
		}
		repl["$"+name] = v
	}
	// Replace longer keys first so $ARGUMENTS is not shadowed by $0.
	keys := make([]string, 0, len(repl))
	for k := range repl {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	const esc = "\x00ESC\x00"
	body = strings.ReplaceAll(body, `\$`, esc)
	for _, k := range keys {
		body = strings.ReplaceAll(body, k, repl[k])
	}
	return strings.ReplaceAll(body, esc, "$")
}

// shellSplit splits a string on whitespace, honoring single and double quotes.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	flush := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return out
}
