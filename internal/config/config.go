// Package config resolves on-disk locations and the agent's system prompt.
package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// configDir returns ~/.config/aigem (honoring XDG_CONFIG_HOME).
func configDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "aigem"), nil
}

// AgentsDir returns ~/.config/aigem/agents, where custom subagent definitions
// (*.md) live. It does not create the directory.
func AgentsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agents"), nil
}

// BotsDir returns ~/.config/aigem/bots, where each subdirectory holds one bot's
// config, memory, and self-authored skills.
func BotsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bots"), nil
}

// SkillRoots returns the directories to scan for skills (each holds
// <name>/SKILL.md subdirs), in priority order: project .skills, global aigem
// skills, project .claude/skills, global Claude skills. aigem locations come
// first so .skills shadows .claude/skills on a name clash. Only existing
// directories are returned. Project locations include cwd, its ancestors up to
// the git root, and nested directories below cwd (monorepo support).
func SkillRoots(cwd string) []SkillRoot {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	// Whether a root belongs to the project is decided here, where the source is
	// known, and never re-derived from the path afterwards: a project rooted at an
	// ancestor of these (a dotfiles repo at $HOME) would otherwise swallow the
	// user's own skills and hold them for approval.
	var cfgSkills, claudeSkills string
	if dir, err := configDir(); err == nil {
		cfgSkills = filepath.Join(dir, "skills")
	}
	if home, err := os.UserHomeDir(); err == nil {
		claudeSkills = filepath.Join(home, ".claude", "skills")
	}
	global := map[string]bool{cfgSkills: true, claudeSkills: true}

	var roots []SkillRoot
	seen := map[string]bool{}
	add := func(dir string, project bool) {
		if dir == "" || seen[dir] || (project && global[dir]) {
			return
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			seen[dir] = true
			roots = append(roots, SkillRoot{Dir: dir, Project: project})
		}
	}

	projectDirs := projectSkillDirs(abs)
	for _, d := range projectDirs {
		add(filepath.Join(d, ".skills"), true)
	}
	add(cfgSkills, false)
	for _, d := range projectDirs {
		add(filepath.Join(d, ".claude", "skills"), true)
	}
	add(claudeSkills, false)
	return roots
}

// SkillRoot is a directory to scan for skills. Project marks it as belonging to
// the checked-out project, and so subject to the project skill trust gate.
type SkillRoot struct {
	Dir     string
	Project bool
}

// projectSkillDirs returns directories that may hold a skills folder: cwd and
// its ancestors up to the project root, plus nested directories below cwd.
func projectSkillDirs(cwd string) []string {
	var dirs []string
	seen := map[string]bool{}
	push := func(d string) {
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	// cwd and ancestors up to (and including) the project root. Outside a git
	// repo that root is cwd itself, so the walk stops there rather than running to
	// / and picking up ancestor skill dirs that belong to no project.
	root := ProjectDir(cwd)
	for d := cwd; ; {
		push(d)
		if d == root || d == filepath.Dir(d) {
			break
		}
		d = filepath.Dir(d)
	}
	// Nested directories below cwd (bounded; skip noisy/vendored trees).
	const maxNested = 2000
	count := 0
	_ = filepath.WalkDir(cwd, func(path string, e fs.DirEntry, err error) error {
		if err != nil || !e.IsDir() || path == cwd {
			return nil
		}
		switch e.Name() {
		case ".git", "node_modules", "vendor", "target", ".venv":
			return filepath.SkipDir
		}
		push(path)
		if count++; count >= maxNested {
			return filepath.SkipAll
		}
		return nil
	})
	return dirs
}

// ProjectDir returns the project root used as a hook's working directory and
// CLAUDE_PROJECT_DIR: the git root walking up from cwd, or cwd itself if none.
func ProjectDir(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return cwd
	}
	if root := gitRoot(abs); root != "" {
		return root
	}
	return abs
}

// SettingsFiles returns the existing settings.json sources holding hook
// definitions, in load order: project .aigem, global aigem, project .claude
// (settings + settings.local), global Claude. Only existing files are returned.
func SettingsFiles(cwd string) []string {
	root := ProjectDir(cwd)
	var files []string
	add := func(p string) {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			files = append(files, p)
		}
	}
	add(filepath.Join(root, ".aigem", "settings.json"))
	if dir, err := configDir(); err == nil {
		add(filepath.Join(dir, "settings.json"))
	}
	add(filepath.Join(root, ".claude", "settings.json"))
	add(filepath.Join(root, ".claude", "settings.local.json"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude", "settings.json"))
	}
	return files
}

// MCPFiles returns the existing JSON sources that may hold an "mcpServers"
// block, highest precedence first: project .aigem, project .claude (settings +
// settings.local), project-root .mcp.json, then global aigem. The caller treats
// the first source to define a server name as the winner, so project beats
// global. Only existing files are returned.
func MCPFiles(cwd string) []string {
	root := ProjectDir(cwd)
	var files []string
	add := func(p string) {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			files = append(files, p)
		}
	}
	add(filepath.Join(root, ".aigem", "settings.json"))
	add(filepath.Join(root, ".claude", "settings.json"))
	add(filepath.Join(root, ".claude", "settings.local.json"))
	add(filepath.Join(root, ".mcp.json"))
	if dir, err := configDir(); err == nil {
		add(filepath.Join(dir, "settings.json"))
	}
	return files
}

// ModelsFiles returns the existing models.json sources that may add or override
// provider/model presets, highest precedence first: project .aigem/models.json,
// then user ~/.config/aigem/models.json. Only existing files are returned.
func ModelsFiles(cwd string) []string {
	var files []string
	p := filepath.Join(ProjectDir(cwd), ".aigem", "models.json")
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		files = append(files, p)
	}
	return append(files, UserModelsFiles()...)
}

// UserModelsFiles returns the user-level models.json if it exists, without the
// project-local file. Callers that must not depend on the current directory (a
// bot's model is pinned once and opened later from another cwd) use this.
func UserModelsFiles() []string {
	dir, err := configDir()
	if err != nil {
		return nil
	}
	p := filepath.Join(dir, "models.json")
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return nil
	}
	return []string{p}
}

// ProjectModelsFile returns the project-local models.json path (it may not
// exist). It is untrusted - it ships with a possibly-cloned repo - so callers
// must not let it redirect a built-in provider's endpoint or auth.
func ProjectModelsFile(cwd string) string {
	return filepath.Join(ProjectDir(cwd), ".aigem", "models.json")
}

// GlobalSettingsFile returns the path to ~/.config/aigem/settings.json (it may
// not exist yet), the global write target for `mcp add --global`.
func GlobalSettingsFile() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ProjectMCPFile returns the project-root .mcp.json path for cwd, the default
// write target for `mcp add`.
func ProjectMCPFile(cwd string) string {
	return filepath.Join(ProjectDir(cwd), ".mcp.json")
}

// StateDir returns ~/.local/state/aigem (honoring XDG_STATE_HOME), creating it.
func StateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "aigem")
	// 0700: this dir holds private state (auth tokens, sessions, oauth) and should
	// not be world-listable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// SystemPrompt returns the user's custom prompt from ~/.config/aigem/SYSTEM.md
// if present, otherwise the built-in default.
func SystemPrompt() string {
	dir, err := configDir()
	if err == nil {
		if data, err := os.ReadFile(filepath.Join(dir, "SYSTEM.md")); err == nil {
			if s := string(data); len(s) > 0 {
				return s
			}
		}
	}
	return DefaultSystemPrompt
}

// ProjectInstructions discovers project-level instruction files and returns
// them formatted for appending to a system prompt, or "" if none are found.
//
// It looks for AGENTS.md / CLAUDE.md and context.md at the git root (walking up
// from cwd; if no repository is found - e.g. the agent runs above the repo - at
// cwd itself), and for cwd/.claude/CLAUDE.md. When both AGENTS.md and CLAUDE.md exist as separate
// files the most recently modified one wins; files that resolve through symlinks
// to the same target are loaded only once (so a root file symlinked to
// .claude/CLAUDE.md is not loaded twice).
func ProjectInstructions(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, f := range instructionFiles(abs) {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if content := strings.TrimSpace(string(data)); content != "" {
			fmt.Fprintf(&b, "### %s\n\n%s\n\n", f, content)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return ""
	}
	return "# Project conventions\n\n" +
		"The instruction files below were found in the working tree and are included here in " +
		"FULL. Follow them. Do NOT re-read these files with tools - their complete contents are " +
		"already provided.\n\n" + out
}

// InstructionPaths returns the paths of the instruction files that
// ProjectInstructions injects, so the caller can mark them as already in
// context (avoiding redundant read_file calls).
func InstructionPaths(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	return instructionFiles(abs)
}

// instructionFiles returns the instruction files to load, in priority order,
// de-duplicated by their canonical (symlink-resolved) path.
func instructionFiles(cwd string) []string {
	var files []string
	seen := map[string]bool{}
	add := func(path string) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return // does not exist
		}
		if info, err := os.Stat(real); err != nil || info.IsDir() {
			return
		}
		if seen[real] {
			return
		}
		seen[real] = true
		files = append(files, path)
	}

	root := gitRoot(cwd)
	if root == "" {
		root = cwd
	}
	if f := pickInstruction(root); f != "" {
		add(f)
	}
	// context.md is a common "map / quick-start" companion to AGENTS.md that
	// projects point at for pointers a coding agent needs up front (where the
	// live config lives, which file holds secrets). AGENTS.md only names it, so
	// inject its full body too rather than relying on the model to open it.
	add(filepath.Join(root, "context.md"))
	add(filepath.Join(cwd, ".claude", "CLAUDE.md"))
	return files
}

// gitRoot walks up from start to the directory containing a .git entry, or ""
// if none is found before the filesystem root.
func gitRoot(start string) string {
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// pickInstruction returns dir/AGENTS.md or dir/CLAUDE.md. If both exist, the
// most recently modified wins (ties favoring AGENTS.md); if one is a symlink to
// the other they share an mtime and either resolves to the same file downstream.
func pickInstruction(dir string) string {
	a := filepath.Join(dir, "AGENTS.md")
	c := filepath.Join(dir, "CLAUDE.md")
	am, aok := modTime(a)
	cm, cok := modTime(c)
	switch {
	case aok && cok:
		if am.Before(cm) {
			return c
		}
		return a
	case aok:
		return a
	case cok:
		return c
	}
	return ""
}

// modTime returns the modification time of path (following symlinks) and whether
// it is an existing regular (non-directory) file.
func modTime(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// DefaultSystemPrompt is the built-in instruction set, adapted for aigem's
// terminal tool loop.
const DefaultSystemPrompt = `You are aigem, a senior software engineering agent working in a terminal on real code.
You operate inside the user's working directory and drive a small set of tools to read and
modify files and run commands. Work autonomously, but read before you write and verify before
you claim. Never say something is done unless you ran it and saw it pass.

When a request mixes a clear, safe part with an ambiguous one, do the clear part right away and
ask only about the rest. Do not gate a trivial, explicitly-requested edit behind a question.
Follow through in the same turn: announcing an action is not a stopping point - make the tool
calls and carry the work to its end. When an implementation choice has a clear, standard default
and is reversible (which library, which of two valid approaches), pick the default, proceed, and
state the assumption in one line - do not halt the task to ask. Reserve questions for choices that
are hard to reverse or that you genuinely cannot resolve on your own.

Operating loop:
1. PLAN FIRST. If the task has more than one step, your very first action is a todo_write call that
   lays out the steps - before any inspection.
2. Inspect the relevant code with read_file, list_dir, grep, and fuzzy_find before acting. Do
   not invent file contents; read them.
3. Make the smallest correct change. Match the naming, structure, and idioms already in the file.
4. Run the language's formatter, linter, and tests with bash. Read the output - do not assume
   success. State results honestly; if something failed or you skipped it, say so.

Planning:
- The FIRST thing you do for any task with more than one step - this includes audits, reviews,
  refactors, multi-file edits, and writing documentation - is call todo_write with the full list of
  steps. Do NOT call read_file, list_dir, or grep before todo_write on such a task. You do not need
  to inspect anything to draft it; if you must look around first, that survey IS step 1 - put it in
  the list and call todo_write before doing it.
- A plan must reach the goal the request states. If the task is to build, fix, implement, or test
  something, the steps must include the actual change AND its verification - never end the plan at
  "explore", "analyze", or "summarize". Those are setup, not the deliverable; if your last step is
  one of them, the plan is too shallow. (A request that only ASKS a question - "what do we need to
  do X?", "how does Y work?" - is the exception: answer it directly, see below.)
- Then work the plan: keep exactly one step in_progress, mark it completed the instant it is done,
  and resend the entire list on every update. Follow it to the end rather than stopping partway.
- Before you give a final answer or ask the user anything, close out the plan: call todo_write to
  mark the finished step completed. Never leave a step in_progress when you have actually done it -
  even if you end by offering follow-up work.
- Do NOT plan a task that just answers one question or makes one small change, even if you read a
  few files to do it - e.g. "what does X do?", "which port does Y use?", "show me file Z", "fix this
  typo". Answer those directly, with no todo_write.

Editing files:
- To change part of an existing file, use edit_file. First read the file, then pass old_string
  copied verbatim (exact whitespace and newlines) and the new_string to replace it with.
- Keep old_string MINIMAL: copy the smallest UNIQUE span that locates the spot - ideally one line,
  rarely more than two or three. A long multi-line old_string is the main cause of failed edits,
  because one wrong space or tab anywhere in it breaks the match. To insert a line, anchor old_string
  on the single unique line you are inserting next to, and repeat that line inside new_string.
- Copy leading indentation EXACTLY as it appears in the read_file output - same characters (tabs vs
  spaces) and the same count. Do not re-indent, align, or tidy the snippet; do not add or drop tabs.
- If an edit returns "old_string not found", the snippet did not match - do NOT resend the same
  edit, it will fail again. Re-read the file, then either anchor on a shorter unique single line, or
  rewrite the whole file with write_file.
- Use write_file only to create a new file or to fully rewrite one: its content replaces the
  ENTIRE file, so never pass a partial snippet to write_file.

Delegation and parallelism:
- For heavy or self-contained work, delegate to a specialized agent with the task tool. It runs
  in its own context and returns a summary, keeping your context lean. Use scout for read-only
  recon, code-writer to implement a change, simplifier to clean up code, and reviewer for an
  independent review. Give the sub-agent a complete, standalone prompt - it cannot see this
  conversation.
- Independent tool calls you put in a SINGLE response run in parallel; calls in separate
  responses run one after another. So to run work concurrently you MUST emit all the calls in one
  response.
- When the user asks for work to be done "in parallel", or asks to act on MULTIPLE independent
  targets (e.g. "review the API of both services", "explore each service"), you MUST emit one
  task call PER target, all together in your VERY NEXT single response - do not run one, wait for
  it, then start the next. One target per task call, all in the same response.

Code standards:
- Correctness first, then clarity, then performance. Handle errors and edge cases explicitly.
- Keep changes focused; do not refactor unrelated code or expand scope without asking.
- Comment only non-obvious "why", never the "what".
- Go: handle every error (no discarding errors), pass context.Context explicitly, run gofmt.

Analysis and findings:
- A finding REQUIRES reading the code. A TODO, a grep hit, a file or directory name
  (reindex-pending), a script, or a plan/progress doc is a LEAD, never a finding. Before you list
  any issue, open the implicated source file with read_file and confirm the behavior in the code.
  Do NOT report a problem sourced only from a TODO marker, a doc, or a grep line - that is the most
  common way to ship a wrong analysis.
- For an audit or a "what should we fix" task, every finding must come from code you read this turn.
  If you have not opened the file behind a claim, either read it now or drop the claim. Grepping for
  TODO/FIXME and paraphrasing plan docs is NOT an analysis - it is a list of leads.
- Calibrate confidence: separate what you verified from what you suspect. Label an unconfirmed claim
  ("likely", "unverified - check X") instead of stating it flat. Never report a bug or vulnerability
  (a path traversal, a race, a data loss) you have not confirmed in the actual code.
- Prefer a few confirmed issues, each citing the path:line you read, over a long list of shallow
  guesses. If you include an unverified lead, mark it clearly as a lead, not a conclusion.

Communication:
- Be concise and direct. Lead with the answer, then minimal detail. Reference code as path:line.
- If a request is risky or suboptimal, say so and offer a better option.
- Do not stop right after announcing intent. End a turn only when the task is done (state what
  you verified) or you genuinely need the user.`
