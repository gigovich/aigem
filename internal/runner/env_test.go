package runner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/pathgrant"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

// project builds a directory Load can be pointed at.
func project(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// load runs Load and fails the test rather than returning an error nobody
// checked, which is the shape every test here wants.
func load(t *testing.T, opts runner.Options) (*runner.Env, []runner.Notice) {
	t.Helper()
	env, notices, err := runner.Load(opts)
	if err != nil {
		t.Fatalf("load %+v: %v", opts, err)
	}
	t.Cleanup(env.Close)
	return env, notices
}

// writeSkill puts a skill under the project's .skills, which is where discovery
// looks for project-local ones.
func writeSkill(t *testing.T, cwd, name, body string) {
	t.Helper()
	dir := filepath.Join(cwd, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFile writes one file, creating its directory.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// globalSettings writes the user-level settings file, whose hooks always run -
// unlike a project's, which are gated on trust.
func globalSettings(t *testing.T, body string) {
	t.Helper()
	dir, err := config.AgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(dir), "settings.json"), body)
}

// sessionStartHook builds a global settings file whose SessionStart hook prints
// out, so a test can choose what the hook tells Load.
func sessionStartHook(t *testing.T, out string) {
	t.Helper()
	cfg := map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "printf", "args": []string{"%s", out}}},
	}}}}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	globalSettings(t, string(body))
}

func noticeTexts(ns []runner.Notice) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Text)
	}
	return out
}

func findNotice(ns []runner.Notice, substr string) (runner.Notice, bool) {
	for _, n := range ns {
		if strings.Contains(n.Text, substr) {
			return n, true
		}
	}
	return runner.Notice{}, false
}

// mustNotice fails unless exactly the expectation holds: a notice containing
// substr exists and its InChat flag says whether the TUI repeats it.
func mustNotice(t *testing.T, ns []runner.Notice, substr string, inChat bool) {
	t.Helper()
	n, ok := findNotice(ns, substr)
	if !ok {
		t.Fatalf("no notice containing %q; got %v", substr, noticeTexts(ns))
	}
	if n.InChat != inChat {
		t.Fatalf("notice %q has InChat=%v, want %v", n.Text, n.InChat, inChat)
	}
}

// mcpProject writes an .mcp.json pointing at an in-process MCP server exposing
// one tool, and returns the project directory. The server is torn down with the
// test.
func mcpProject(t *testing.T) string {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "v1"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo back text"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
			Text string `json:"text"`
		}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
		})
	// A server is entitled to know which build of which client it is talking to;
	// it is the only place the version Load was given is observable.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "whoami", Description: "report the client"},
		func(_ context.Context, req *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			info := req.Session.InitializeParams().ClientInfo
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: info.Name + " " + info.Version}},
			}, nil, nil
		})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	srvHTTP := httptest.NewServer(handler)
	t.Cleanup(srvHTTP.Close)

	cwd := project(t)
	writeFile(t, filepath.Join(cwd, ".mcp.json"),
		`{"mcpServers":{"srv":{"url":"`+srvHTTP.URL+`"}}}`)
	return cwd
}

func TestLoadDiscoversTrustedProjectSkills(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	env, notices := load(t, runner.Options{Cwd: cwd})

	if env.Pending != nil {
		t.Fatalf("approved project should have nothing pending, got %+v", env.Pending)
	}
	if got := env.Skills.ModelNames(); len(got) != 1 || got[0] != "greet" {
		t.Fatalf("expected the greet skill to be discovered, got %v (notices %v)",
			got, noticeTexts(notices))
	}
	p, _ := env.SystemPrompt()
	if !strings.Contains(p, "say hi nicely") {
		t.Fatalf("expected the skill catalog in the system prompt, got %q", p)
	}
}

// Discovery drops untrusted project-local skills silently, which is
// indistinguishable from the project having none. Load has to hand the caller
// what was withheld, or nobody can offer to approve it.
func TestLoadWithholdsUntrustedProjectSkills(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")

	env, _ := load(t, runner.Options{Cwd: cwd})

	if len(env.Skills.ModelNames()) != 0 {
		t.Fatalf("untrusted skills must not be loaded, got %v", env.Skills.ModelNames())
	}
	if env.Pending == nil {
		t.Fatal("expected the withheld skill to be reported as pending")
	}
	if len(env.Pending.Names) != 1 || env.Pending.Names[0] != "greet" {
		t.Fatalf("expected greet to be pending, got %v", env.Pending.Names)
	}
}

// Each --trust-project-* option approves its own capability and nothing else.
func TestTrustOptionsGateOneCapabilityEach(t *testing.T) {
	settings := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"true"}]}]}}`

	newProject := func(t *testing.T) string {
		cwd := project(t)
		writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi\n---\nHello.\n")
		writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"), settings)
		writeFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"srv":{"command":"echo"}}}`)
		return cwd
	}

	t.Run("nothing trusted", func(t *testing.T) {
		env, notices := load(t, runner.Options{Cwd: newProject(t)})
		if len(env.Skills.ModelNames()) != 0 {
			t.Fatal("skills loaded without --trust-project-skills")
		}
		if !env.Hooks.HasUntrustedProjectHooks() {
			t.Fatal("project hooks reported as trusted without --trust-project-hooks")
		}
		mustNotice(t, notices, "mcp config: ", false)
	})

	t.Run("skills only", func(t *testing.T) {
		env, notices := load(t, runner.Options{Cwd: newProject(t), TrustProjectSkills: true})
		if len(env.Skills.ModelNames()) != 1 {
			t.Fatalf("skills not trusted: %v", env.Skills.ModelNames())
		}
		if !env.Hooks.HasUntrustedProjectHooks() {
			t.Fatal("--trust-project-skills also trusted the hooks")
		}
		if _, ok := findNotice(notices, "mcp config: "); !ok {
			t.Fatal("--trust-project-skills also trusted the MCP servers")
		}
	})

	t.Run("hooks only", func(t *testing.T) {
		env, notices := load(t, runner.Options{Cwd: newProject(t), TrustProjectHooks: true})
		if env.Hooks.HasUntrustedProjectHooks() {
			t.Fatal("--trust-project-hooks did not trust the project hooks")
		}
		if len(env.Skills.ModelNames()) != 0 {
			t.Fatal("--trust-project-hooks also trusted the skills")
		}
		if _, ok := findNotice(notices, "mcp config: "); !ok {
			t.Fatal("--trust-project-hooks also trusted the MCP servers")
		}
	})

	t.Run("mcp only", func(t *testing.T) {
		env, notices := load(t, runner.Options{Cwd: newProject(t), TrustProjectMCP: true})
		if _, ok := findNotice(notices, "MCP server"); ok {
			t.Fatalf("--trust-project-mcp left the servers pending: %v", noticeTexts(notices))
		}
		if len(env.Skills.ModelNames()) != 0 {
			t.Fatal("--trust-project-mcp also trusted the skills")
		}
		if !env.Hooks.HasUntrustedProjectHooks() {
			t.Fatal("--trust-project-mcp also trusted the hooks")
		}
	})
}

// A project whose settings file will not parse is still a project someone wants
// to work in, so Load reports it and carries on with the hooks it could read.
func TestLoadReportsBrokenHookConfig(t *testing.T) {
	cwd := project(t)
	writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"), "{not json")

	env, notices := load(t, runner.Options{Cwd: cwd})

	mustNotice(t, notices, "hook config:", false)
	if env.Hooks == nil {
		t.Fatal("a broken config must still leave a hooks runner behind")
	}
}

// The out-of-scope notice is the entire reason that code exists: a skill
// directory above the project root is loaded by nobody and offered for approval
// by nobody, so saying so is all that stands between it and silence.
func TestLoadReportsSkillDirectoriesAboveTheProject(t *testing.T) {
	cwd := project(t)
	outside := filepath.Join(filepath.Dir(cwd), ".skills", "stray")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(outside))) })

	_, notices := load(t, runner.Options{Cwd: cwd})

	mustNotice(t, notices, "are outside this project and were not loaded", true)
}

// Discovery cannot say whether project-local skills were withheld when it
// cannot read the trust store, and a session that quietly has no skills is the
// symptom nobody diagnoses.
func TestLoadReportsAnUnreadableTrustStore(t *testing.T) {
	state, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(state, "project-trust.json"), "{not json")
	t.Cleanup(func() { _ = os.Remove(filepath.Join(state, "project-trust.json")) })

	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi\n---\nHello.\n")
	_, notices := load(t, runner.Options{Cwd: cwd})

	mustNotice(t, notices, "could not evaluate project skill trust:", true)
}

// A custom agents directory that will not load costs the session its subagents,
// which is not something to discover from the delegation tool's silence.
func TestLoadReportsUnreadableCustomAgents(t *testing.T) {
	dir, err := config.AgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	// A file where the directory belongs: readable, and not a directory.
	writeFile(t, dir, "not a directory\n")
	t.Cleanup(func() { _ = os.Remove(dir) })

	_, notices := load(t, runner.Options{Cwd: project(t)})

	mustNotice(t, notices, "could not load custom agents:", false)
}

// A configured server that cannot be reached is skipped rather than taking
// startup down, and the person is told which one went missing.
func TestLoadReportsAnMCPServerThatWillNotStart(t *testing.T) {
	cwd := project(t)
	writeFile(t, filepath.Join(cwd, ".mcp.json"),
		`{"mcpServers":{"broken":{"command":"aigem-no-such-command-exists"}}}`)

	env, notices := load(t, runner.Options{Cwd: cwd, TrustProjectMCP: true})

	if _, ok := findNotice(notices, "broken"); !ok {
		t.Fatalf("expected a warning naming the server that failed, got %v", noticeTexts(notices))
	}
	if env.MCP == nil {
		t.Fatal("a failed server must still leave a manager behind")
	}
}

// The caller decides how a notice is presented, so Load must not have decided
// already by baking a prefix into the text.
func TestNoticesCarryNoPrefix(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "broken", "---\nname: [unterminated\n---\nnothing\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"), "{not json")

	_, notices := load(t, runner.Options{Cwd: cwd})

	if len(notices) < 2 {
		t.Fatalf("expected notices for an unparsable skill and a broken settings file, got %v",
			noticeTexts(notices))
	}
	for _, n := range notices {
		if strings.HasPrefix(n.Text, "warning") {
			t.Fatalf("notice %q carries a presentation prefix", n.Text)
		}
	}
	// A skipped skill is raised before the alt screen and has to be repeated in
	// it; a config warning is not.
	mustNotice(t, notices, "skipped skill:", true)
	mustNotice(t, notices, "hook config:", false)
}

// The SessionStart hook is the one thing in Load that runs a person's own
// commands, and all three of its outputs are load-bearing.
func TestLoadRunsTheSessionStartHook(t *testing.T) {
	sessionStartHook(t, `{"systemMessage":"MSG","sessionTitle":"TITLE",`+
		`"hookSpecificOutput":{"additionalContext":"HOOK CONTEXT"}}`)

	env, _ := load(t, runner.Options{Cwd: project(t)})

	if env.SessionTitle != "TITLE" {
		t.Fatalf("SessionTitle = %q, want TITLE", env.SessionTitle)
	}
	if env.SystemMessage != "MSG" {
		t.Fatalf("SystemMessage = %q, want MSG", env.SystemMessage)
	}
	p, _ := env.SystemPrompt()
	if !strings.Contains(p, "HOOK CONTEXT") {
		t.Fatalf("expected the hook's context in the system prompt, got %q", p)
	}
}

// A hook that never returns must delay startup, not freeze it.
func TestLoadBoundsTheSessionStartHook(t *testing.T) {
	defer runner.SetSessionStartTimeout(200 * time.Millisecond)()
	cfg := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"sleep","args":["60"]}]}]}}`
	globalSettings(t, cfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		env, _, err := runner.Load(runner.Options{Cwd: project(t)})
		if err == nil {
			env.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Load did not return while a SessionStart hook was still sleeping")
	}
}

// A working directory that cannot be resolved is a startup that was always
// going to fail, and it must fail before the SessionStart hook has run anyone's
// commands or a single MCP server has been started.
func TestLoadRefusesAnUnresolvableWorkingDirectory(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cfg := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch",` +
		`"args":["` + marker + `"]}]}]}}`
	globalSettings(t, cfg)

	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	env, _, err := runner.Load(runner.Options{Cwd: "."})
	if err == nil {
		env.Close()
		t.Fatal("Load accepted a working directory that no longer exists")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the SessionStart hook ran on a startup that was going to fail anyway")
	}
}

// Two conversations must not share a registry: the delegation and skill tools
// are bound to one session's confirmation function.
func TestNewToolsGivesEachSessionItsOwnRegistry(t *testing.T) {
	env, _ := load(t, runner.Options{Cwd: project(t)})

	a, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	b, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("NewTools handed out the same registry twice")
	}
	a.Register(markerTool{})
	if _, ok := b.Get("marker"); ok {
		t.Fatal("registering into one session's registry reached another's")
	}
	if a.Root() != b.Root() {
		t.Fatalf("both registries are the same sandbox: %q vs %q", a.Root(), b.Root())
	}
}

// The MCP tools go into every registry, not just the first: a second session
// has to reach the servers the manager already dialled.
func TestNewToolsRegistersMCPToolsInEveryRegistry(t *testing.T) {
	env, notices := load(t, runner.Options{Cwd: mcpProject(t), TrustProjectMCP: true})
	if len(notices) != 0 {
		t.Fatalf("unexpected notices: %v", noticeTexts(notices))
	}

	for i := 1; i <= 2; i++ {
		reg, err := env.NewTools()
		if err != nil {
			t.Fatal(err)
		}
		tool, ok := reg.Get("mcp__srv__echo")
		if !ok {
			t.Fatalf("registry %d has no MCP tool: %v", i, reg.Names())
		}
		// Registered is not the same as reachable: the second registry has to
		// share the connection the first one used, not a stale adapter.
		out, err := tool.Run(context.Background(), json.RawMessage(`{"text":"hi"}`))
		if err != nil {
			t.Fatalf("registry %d: calling the MCP tool: %v", i, err)
		}
		if out != "hi" {
			t.Fatalf("registry %d: MCP tool returned %q, want hi", i, out)
		}
	}
}

// A server is told which client it is talking to in the initialize handshake,
// and a client that misreports its version is a support case nobody can read.
func TestLoadIdentifiesThisClientToMCPServers(t *testing.T) {
	env, _ := load(t, runner.Options{
		Cwd: mcpProject(t), TrustProjectMCP: true, Version: "v9.9.9-test",
	})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("mcp__srv__whoami")
	if !ok {
		t.Fatalf("no whoami tool: %v", reg.Names())
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v9.9.9-test") {
		t.Fatalf("the server was told %q, want the version Load was given", out)
	}
}

// Close is the only thing that reaps what Load dialled.
func TestCloseShutsTheMCPServersDown(t *testing.T) {
	env, _, err := runner.Load(runner.Options{Cwd: mcpProject(t), TrustProjectMCP: true})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("mcp__srv__echo")
	if !ok {
		t.Fatalf("no MCP tool to call: %v", reg.Names())
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("the tool should work before Close: %v", err)
	}

	env.Close()

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"text":"hi"}`)); err == nil {
		t.Fatal("the MCP session survived Env.Close")
	}
}

func TestNewToolsRegistersEverySearchToolOnlyWhenConfigured(t *testing.T) {
	cwd := project(t)
	names := []string{"web_search", "open_url", "browser_action"}

	off, _ := load(t, runner.Options{Cwd: cwd})
	reg, err := off.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := reg.Get(n); ok {
			t.Fatalf("an unconfigured search backend registered %s", n)
		}
	}

	// The browser backend is the one that carries all three; brave carries only
	// the search tool, so a test that used it would never notice the other two.
	on, _ := load(t, runner.Options{Cwd: cwd, Search: browserSearch()})
	reg, err = on.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := reg.Get(n); !ok {
			t.Fatalf("expected %s once search is configured, got %v", n, reg.Names())
		}
	}
}

// browserSearch is a configured search backend that provides every search tool.
func browserSearch() search.Config {
	return search.Config{Provider: "browser", Browser: &search.BrowserConfig{Engine: "duckduckgo"}}
}

// Persisted path grants are the front-end's decision, not the sandbox's: an
// unattended run must not inherit a directory a person approved by hand.
func TestNewToolsLeavesPathGrantsOff(t *testing.T) {
	cwd := project(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	writeFile(t, secret, "top secret\n")

	env, _ := load(t, runner.Options{Cwd: cwd})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	if err := pathgrant.Add(reg.Root(), outside); err != nil {
		t.Fatal(err)
	}

	tool, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("no read_file tool")
	}
	args, err := json.Marshal(map[string]any{"path": secret})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := tool.Run(context.Background(), args); err == nil {
		t.Fatalf("a granted directory was readable straight out of NewTools: %q", out)
	}

	// The same registry with grants enabled - what a front-end does - reads it.
	reg.SetPathGrants(true)
	if _, err := tool.Run(context.Background(), args); err != nil {
		t.Fatalf("the grant should apply once the front-end enables grants: %v", err)
	}
}

// The prompt carries the instruction files, and the session that got them must
// not spend a tool call reading them back.
func TestSystemPromptReportsTheFilesItInjected(t *testing.T) {
	cwd := project(t)
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "PROJECT RULES\n")
	env, _ := load(t, runner.Options{Cwd: cwd})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}

	if out := readFile(t, reg, "AGENTS.md"); !strings.Contains(out, "PROJECT RULES") {
		t.Fatalf("expected the file to read normally before the prompt is built, got %q", out)
	}
	p, injected := env.SystemPrompt()
	if !strings.Contains(p, "PROJECT RULES") {
		t.Fatalf("expected the project instructions in the prompt, got %q", p)
	}
	if len(injected) == 0 {
		t.Fatal("the prompt injected AGENTS.md but reported no files to mark")
	}
	reg.MarkInContext(injected)
	if out := readFile(t, reg, "AGENTS.md"); !strings.Contains(out, "already included") {
		t.Fatalf("expected read_file to report the file as already in context, got %q", out)
	}
}

// A project with no instruction files has nothing to mark, and a caller that
// marks what it is given must not be handed the whole project.
func TestSystemPromptInjectsNothingWithoutInstructionFiles(t *testing.T) {
	env, _ := load(t, runner.Options{Cwd: project(t)})
	if _, injected := env.SystemPrompt(); len(injected) != 0 {
		t.Fatalf("nothing was injected, but the prompt reported %v", injected)
	}
}

// /new rebuilds the prompt so an edit takes effect without a restart. That only
// works if the files are re-read rather than snapshotted at load.
func TestSystemPromptRereadsInstructionFiles(t *testing.T) {
	cwd := project(t)
	path := filepath.Join(cwd, "AGENTS.md")
	writeFile(t, path, "FIRST RULE\n")
	env, _ := load(t, runner.Options{Cwd: cwd})

	if p, _ := env.SystemPrompt(); !strings.Contains(p, "FIRST RULE") {
		t.Fatalf("expected the first version, got %q", p)
	}
	writeFile(t, path, "SECOND RULE\n")
	p, _ := env.SystemPrompt()
	if !strings.Contains(p, "SECOND RULE") || strings.Contains(p, "FIRST RULE") {
		t.Fatalf("expected the edited instructions, got %q", p)
	}
	// Project is the load-time snapshot, and is deliberately not re-read: it is
	// what a subagent was told about the project when the session started.
	if !strings.Contains(env.Project, "FIRST RULE") {
		t.Fatalf("expected Project to hold the load-time text, got %q", env.Project)
	}
}

// Every block the prompt is made of, in the order the model reads them. The
// order is asserted because a catalog that arrives before the conventions it is
// meant to obey is a different prompt, and nothing else would notice.
func TestSystemPromptCarriesEveryBlockInOrder(t *testing.T) {
	sessionStartHook(t, `{"hookSpecificOutput":{"additionalContext":"HOOK CONTEXT"}}`)
	cwd := mcpProject(t)
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "PROJECT RULES\n")
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nHello.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	env, _ := load(t, runner.Options{
		Cwd:             cwd,
		TrustProjectMCP: true,
		Search:          browserSearch(),
	})
	p, _ := env.SystemPrompt()

	if base := config.SystemPrompt(); base == "" || !strings.HasPrefix(p, base) {
		t.Fatalf("the prompt does not start with the base instructions: %q", p)
	}
	blocks := []struct{ name, marker string }{
		{"date and time", "# Current date and time"},
		{"project instructions", "PROJECT RULES"},
		{"delegation", "# Delegation and parallelism"},
		{"skill catalog", "say hi nicely"},
		{"search", "web_search"},
		{"mcp", "mcp__srv__echo"},
		{"hook context", "HOOK CONTEXT"},
	}
	at := -1
	for _, b := range blocks {
		i := strings.Index(p, b.marker)
		if i < 0 {
			t.Fatalf("the %s block is missing from the prompt:\n%s", b.name, p)
		}
		if i < at {
			t.Fatalf("the %s block appears before the one that should precede it:\n%s", b.name, p)
		}
		at = i
	}
}

// readFile runs the read_file tool, which is how the in-context marking is
// observable at all.
func readFile(t *testing.T, reg *tools.Registry, path string) string {
	t.Helper()
	tool, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("no read_file tool in the registry")
	}
	args, err := json.Marshal(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("read_file %s: %v", path, err)
	}
	return out
}

// markerTool is a tool with no behaviour, for asserting which registry a
// registration landed in.
type markerTool struct{}

func (markerTool) Name() string            { return "marker" }
func (markerTool) Description() string     { return "marker" }
func (markerTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (markerTool) NeedsConfirm() bool      { return false }
func (markerTool) Run(context.Context, json.RawMessage) (string, error) {
	return "", nil
}
