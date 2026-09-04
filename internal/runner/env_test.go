package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
	env, notices, err := runner.Load(context.Background(), opts)
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
//
// The sandbox HOME belongs to the whole test binary, not to one test, so this
// removes the file again. Leaving a hook behind makes every later Load in the
// package run it, which is a twenty-fold swing in the package's running time
// depending on the order the tests happen to be in.
func globalSettings(t *testing.T, body string) {
	t.Helper()
	dir, err := config.AgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(dir), "settings.json")
	writeFile(t, path, body)
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("removing the global settings file: %v", err)
		}
	})
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
	// Cleanups run last-registered-first, so this pair unwinds as: the test's own
	// Env.Close, then the client connections, then the server. Without the middle
	// step a session the Env failed to reap holds the streamable-HTTP GET open
	// and Server.Close waits for it forever - which turns a failing assertion
	// into a hung package, and stops the test written against exactly that
	// failure from ever running.
	t.Cleanup(srvHTTP.Close)
	t.Cleanup(srvHTTP.CloseClientConnections)

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

// A capability discovery withheld is raised where it is discovered, not left
// for the caller to work out afterwards - and marked so a front-end that can
// ask does that instead of printing.
func TestWithheldCapabilitiesAreRaisedWhereTheyAreFound(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi\n---\nHello.\n")
	writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"),
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"true"}]}]}}`)
	writeFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"srv":{"command":"echo"}}}`)

	_, notices := load(t, runner.Options{Cwd: cwd})

	skills, ok := findNotice(notices, "project-local skills in ")
	if !ok {
		t.Fatalf("withheld skills were not reported: %v", noticeTexts(notices))
	}
	if !skills.Askable {
		t.Error("a withheld skill is something a front-end can offer to approve")
	}
	if skills.InChat {
		t.Error("the TUI asks about withheld skills with an overlay, not a chat line")
	}
	hooks, ok := findNotice(notices, "project-local hooks present but untrusted")
	if !ok {
		t.Fatalf("withheld hooks were not reported: %v", noticeTexts(notices))
	}
	if !hooks.Askable {
		t.Error("withheld hooks are something a front-end can offer to approve")
	}

	// Position is the point: skills are discovered before the MCP servers are
	// dialled, and the person waiting for them must not learn why last.
	var skillsAt, mcpAt = -1, -1
	for i, n := range notices {
		switch {
		case strings.Contains(n.Text, "project-local skills in "):
			skillsAt = i
		case strings.Contains(n.Text, "mcp config: "):
			mcpAt = i
		}
	}
	if mcpAt < 0 {
		t.Fatalf("the fixture raised no MCP notice: %v", noticeTexts(notices))
	}
	if skillsAt > mcpAt {
		t.Fatalf("the withheld-skills notice is raised after the MCP dial: %v", noticeTexts(notices))
	}
}

// An approved project has nothing to offer, so nothing is asked.
func TestNothingIsAskableWhenNothingIsWithheld(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi\n---\nHello.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	_, notices := load(t, runner.Options{Cwd: cwd})
	for _, n := range notices {
		if n.Askable {
			t.Fatalf("nothing was withheld, but %q asks to be approved", n.Text)
		}
	}
}

// Close is the end of the environment, not a pause in it: the MCP manager goes
// on listing tools it has torn down, so a registry built afterwards would carry
// a full catalog whose every call fails.
func TestNewToolsRefusesAfterClose(t *testing.T) {
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: mcpProject(t), TrustProjectMCP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.NewTools(); err != nil {
		t.Fatalf("NewTools before Close: %v", err)
	}

	env.Close()
	env.Close() // twice is safe

	if _, err := env.NewTools(); err == nil {
		t.Fatal("NewTools handed out a registry from a closed environment")
	}
}

// A --trust-project-* flag that cannot record its decision has to say so: the
// person asked for something durable, and silence would let them believe they
// got it while every later start withholds the same capabilities again.
func TestLoadReportsTrustThatCouldNotBePersisted(t *testing.T) {
	// The whole fixture is a directory root is allowed to write anyway.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so nothing here can fail to be persisted")
	}
	state, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// Readable, so everything that only consults trust still works; unwritable,
	// so recording a decision fails.
	if err := os.Chmod(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(state, 0o700); err != nil {
			t.Errorf("restoring the state directory: %v", err)
		}
	})

	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi\n---\nHello.\n")
	writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"),
		`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"true"}]}]}}`)

	_, notices := load(t, runner.Options{
		Cwd: cwd, TrustProjectSkills: true, TrustProjectHooks: true,
	})

	mustNotice(t, notices, "could not approve project skills:", false)
	mustNotice(t, notices, "could not persist project trust:", false)
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

	mustNotice(t, notices, "broken", false)
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
// commands, and all three of its outputs are load-bearing. So is what it is
// told: a hook branches on the source it was started for.
func TestLoadRunsTheSessionStartHook(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hook.sh")
	writeFile(t, script, "#!/bin/sh\n"+
		`src=$(sed -n 's/.*"source":"\([^"]*\)".*/\1/p')`+"\n"+
		`printf '{"systemMessage":"MSG","sessionTitle":"TITLE",`+
		`"hookSpecificOutput":{"additionalContext":"HOOK CONTEXT src[%s]"}}' "$src"`+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	globalSettings(t, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"`+
		script+`"}]}]}}`)

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
	if !strings.Contains(p, "src[startup]") {
		t.Fatalf("the hook was not told which source it was started for, got %q", p)
	}
}

// A hook is told the directory it is running for, and the hooks runner stores
// that string exactly as it was handed one - it does not resolve it again. So
// this is the one consumer for which passing the caller's relative path instead
// of the resolved root is not the same thing: a person's hook would be handed
// "." and have to guess what it meant.
func TestLoadGivesHooksTheResolvedDirectory(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hook.sh")
	writeFile(t, script, "#!/bin/sh\n"+
		`d=$(sed -n 's/.*"cwd":"\([^"]*\)".*/\1/p')`+"\n"+
		`printf '{"hookSpecificOutput":{"additionalContext":"HOOK CWD[%s]"}}' "$d"`+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	globalSettings(t, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"`+
		script+`"}]}]}}`)

	cwd := project(t)
	t.Chdir(cwd)
	env, _ := load(t, runner.Options{Cwd: "."})

	p, _ := env.SystemPrompt()
	const marker = "HOOK CWD["
	i := strings.Index(p, marker)
	if i < 0 {
		t.Fatalf("the hook never reported its directory: %q", p)
	}
	rest := p[i+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatalf("the hook's report is not terminated: %q", rest)
	}
	if got := rest[:end]; !filepath.IsAbs(got) {
		t.Fatalf("the hook was handed %q, want the resolved project directory", got)
	}
}

// A hook that never returns must delay startup, not freeze it.
func TestLoadBoundsTheSessionStartHook(t *testing.T) {
	defer runner.SetSessionStartTimeout(200 * time.Millisecond)()
	cfg := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"sleep","args":["5"]}]}]}}`
	globalSettings(t, cfg)

	// Resolved on this goroutine: t.TempDir after the test has finished panics,
	// and on the failure path below the goroutine outlives the test.
	cwd := project(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env, _, err := runner.Load(context.Background(), runner.Options{Cwd: cwd})
		if err == nil {
			env.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Load did not return while a SessionStart hook was still sleeping")
	}
}

func TestLoadCancellationStopsSessionStartHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	script := filepath.Join(t.TempDir(), "hook.sh")
	writeFile(t, script, "#!/bin/sh\n"+
		"touch "+marker+"\n"+
		"sleep 30\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	globalSettings(t, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"`+script+`"}]}]}}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cwd := project(t)
	type result struct {
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		_, _, err := runner.Load(ctx, runner.Options{Cwd: cwd})
		resultCh <- result{err: err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SessionStart hook did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-resultCh:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Load error = %v, want context.Canceled", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Load did not stop after context cancellation")
	}
}

func TestLoadCancellationStopsMCPStartup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	script := filepath.Join(t.TempDir(), "mcp.sh")
	writeFile(t, script, "#!/bin/sh\n"+
		"touch "+marker+"\n"+
		"sleep 30\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := project(t)
	writeFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"slow":{"command":"`+script+`"}}}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		_, _, err := runner.Load(ctx, runner.Options{Cwd: cwd, TrustProjectMCP: true})
		resultCh <- result{err: err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MCP server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-resultCh:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Load error = %v, want context.Canceled", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Load did not stop after context cancellation")
	}
}

// A working directory that cannot be resolved is a startup that was always
// going to fail, and it must fail before the SessionStart hook has run anyone's
// commands or a single MCP server has been started.
//
// Both markers are global, not project-local: a project-local one would be
// withheld for lack of trust under any mutation that breaks the root, and its
// absence would prove nothing. The control at the end is what makes an absent
// marker mean something at all.
func TestLoadRefusesAnUnresolvableWorkingDirectory(t *testing.T) {
	markers := t.TempDir()
	hookRan := filepath.Join(markers, "hook-ran")
	serverStarted := filepath.Join(markers, "server-started")
	cfg := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch",` +
		`"args":["` + hookRan + `"]}]}]},` +
		`"mcpServers":{"marker":{"command":"touch","args":["` + serverStarted + `"]}}}`
	globalSettings(t, cfg)

	good := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: "."})
	if err == nil {
		env.Close()
		t.Fatal("Load accepted a working directory that no longer exists")
	}
	if _, statErr := os.Stat(hookRan); statErr == nil {
		t.Error("the SessionStart hook ran on a startup that was going to fail anyway")
	}
	if _, statErr := os.Stat(serverStarted); statErr == nil {
		t.Error("an MCP server was started on a startup that was going to fail anyway")
	}

	// The control. An absent marker says nothing on its own - a fixture that
	// never fires would leave one absent too - so the same configuration is
	// shown to produce both when the working directory is fine.
	t.Chdir(good)
	load(t, runner.Options{Cwd: good})
	if _, statErr := os.Stat(hookRan); statErr != nil {
		t.Fatalf("the fixture never fires: the hook did not run for a good working "+
			"directory either (%v)", statErr)
	}
	if _, statErr := os.Stat(serverStarted); statErr != nil {
		t.Fatalf("the fixture never fires: no MCP server was started for a good working "+
			"directory either (%v)", statErr)
	}
}

// The root every discovery resolves from is stored resolved, so it does not
// depend on the process working directory later moving.
func TestLoadResolvesTheWorkingDirectory(t *testing.T) {
	cwd := project(t)
	t.Chdir(cwd)

	env, _ := load(t, runner.Options{Cwd: "."})

	if !filepath.IsAbs(env.Cwd) {
		t.Fatalf("Env.Cwd = %q, want an absolute path", env.Cwd)
	}
}

// The error names what could not be resolved and keeps the cause, because the
// person reading it is looking at a shell that will not say why either.
func TestLoadErrorNamesTheDirectoryAndKeepsTheCause(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	_, _, err := runner.Load(context.Background(), runner.Options{Cwd: "."})
	if err == nil {
		t.Fatal("Load accepted a working directory that no longer exists")
	}
	if !strings.Contains(err.Error(), `"."`) {
		t.Fatalf("the error does not name the directory: %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the error dropped its cause: %v", err)
	}
}

// Notices are handed over in the order they were raised: the caller prints them
// in that order, and a person reading a terminal reads a sequence.
func TestNoticesKeepTheOrderTheyWereRaisedIn(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "broken", "---\nname: [unterminated\n---\nnothing\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"), "{not json")

	_, notices := load(t, runner.Options{Cwd: cwd})

	skipped, hookCfg := -1, -1
	for i, n := range notices {
		switch {
		case strings.Contains(n.Text, "skipped skill:"):
			skipped = i
		case strings.Contains(n.Text, "hook config:"):
			hookCfg = i
		}
	}
	if skipped < 0 || hookCfg < 0 {
		t.Fatalf("expected both notices, got %v", noticeTexts(notices))
	}
	// Skills are discovered before the hooks are read, and the list says so.
	if skipped > hookCfg {
		t.Fatalf("notices are out of discovery order: %v", noticeTexts(notices))
	}
}

// The point of Notify is WHEN it is called. Load dials the MCP servers and runs
// the SessionStart hook, either of which can take tens of seconds, so a
// callback that only fires once Load returns leaves a terminal silent for all
// of it - which is the regression this exists to prevent, and which an
// assertion about the list alone would not see.
func TestNotifyFiresBeforeLoadReturns(t *testing.T) {
	defer runner.SetSessionStartTimeout(2 * time.Second)()
	globalSettings(t, `{"hooks":{"SessionStart":[{"hooks":[`+
		`{"type":"command","command":"sleep","args":["6"]}]}]}}`)

	cwd := project(t)
	// Raised during skill discovery, long before the hook runs.
	writeSkill(t, cwd, "broken", "---\nname: [unterminated\n---\nnothing\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	first := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env, _, err := runner.Load(context.Background(), runner.Options{
			Cwd: cwd,
			Notify: func(runner.Notice) {
				select {
				case first <- struct{}{}:
				default:
				}
			},
		})
		if err == nil {
			env.Close()
		}
	}()

	select {
	case <-first:
	case <-time.After(90 * time.Second):
		t.Fatal("no notice arrived at all")
	}
	select {
	case <-done:
		t.Fatal("Load had already returned when the first notice arrived: the callback is " +
			"batched at the end, and a caller printing from it stays silent until then")
	default:
	}

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Load never returned")
	}
}

// Every notice reaches Notify, in the order the returned list holds them.
func TestNotifyIsCalledAsNoticesAreRaised(t *testing.T) {
	cwd := project(t)
	writeFile(t, filepath.Join(cwd, ".aigem", "settings.json"), "{not json")
	writeSkill(t, cwd, "broken", "---\nname: [unterminated\n---\nnothing\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	var streamed []runner.Notice
	env, notices, err := runner.Load(context.Background(), runner.Options{
		Cwd:    cwd,
		Notify: func(n runner.Notice) { streamed = append(streamed, n) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.Close)

	if len(streamed) != len(notices) {
		t.Fatalf("Notify saw %d notices, the returned list has %d", len(streamed), len(notices))
	}
	for i := range notices {
		if streamed[i] != notices[i] {
			t.Fatalf("notice %d differs: streamed %+v, returned %+v", i, streamed[i], notices[i])
		}
	}
	if len(streamed) == 0 {
		t.Fatal("the fixture raised no notices at all")
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
func TestLoadUsesSeparateProjectRuntimesForDifferentTrustRoots(t *testing.T) {
	firstRoot := project(t)
	secondRoot := project(t)
	if err := os.Mkdir(filepath.Join(firstRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(secondRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, firstNotices, err := runner.Load(context.Background(), runner.Options{Cwd: firstRoot})
	if err != nil {
		t.Fatal(err)
	}
	second, secondNotices, err := runner.Load(context.Background(), runner.Options{Cwd: secondRoot})
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	if len(firstNotices) != 0 || len(secondNotices) != 0 {
		t.Fatalf("unexpected notices: first=%v second=%v", noticeTexts(firstNotices), noticeTexts(secondNotices))
	}
	if first.MCP == second.MCP {
		t.Fatal("different trust roots shared an MCP manager")
	}
	if first.Agents == second.Agents {
		t.Fatal("different trust roots shared an agent registry")
	}
}

func TestLoadKeepsWorktreeEnvironmentStateSeparate(t *testing.T) {
	root := project(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(root, "one")
	two := filepath.Join(root, "two")
	if err := os.MkdirAll(one, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(two, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, one, "one-only", "---\nname: one-only\ndescription: one worktree\n---\nOne.\n")
	writeSkill(t, two, "two-only", "---\nname: two-only\ndescription: two worktree\n---\nTwo.\n")
	writeFile(t, filepath.Join(one, ".claude", "CLAUDE.md"), "ONE INSTRUCTIONS\n")
	writeFile(t, filepath.Join(two, ".claude", "CLAUDE.md"), "TWO INSTRUCTIONS\n")
	first, firstNotices := load(t, runner.Options{Cwd: one})
	second, secondNotices := load(t, runner.Options{Cwd: two})
	if len(firstNotices) == 0 || len(secondNotices) == 0 {
		t.Fatalf("expected pending-skill notices: first=%v second=%v", noticeTexts(firstNotices), noticeTexts(secondNotices))
	}
	if first.Cwd == second.Cwd {
		t.Fatalf("worktree environments have the same cwd: %q", first.Cwd)
	}
	if first.MCP != second.MCP {
		t.Fatal("worktrees in one project did not share the project runtime")
	}
	if first.Pending == nil || len(first.Pending.Names) != 1 || first.Pending.Names[0] != "one-only" {
		t.Fatalf("first worktree pending skills = %+v, want only one-only", first.Pending)
	}
	if second.Pending == nil || len(second.Pending.Names) != 1 || second.Pending.Names[0] != "two-only" {
		t.Fatalf("second worktree pending skills = %+v, want only two-only", second.Pending)
	}
	firstPrompt, _ := first.SystemPrompt()
	secondPrompt, _ := second.SystemPrompt()
	if !strings.Contains(firstPrompt, "ONE INSTRUCTIONS") || strings.Contains(firstPrompt, "TWO INSTRUCTIONS") {
		t.Fatalf("first worktree instructions mixed: %q", firstPrompt)
	}
	if !strings.Contains(secondPrompt, "TWO INSTRUCTIONS") || strings.Contains(secondPrompt, "ONE INSTRUCTIONS") {
		t.Fatalf("second worktree instructions mixed: %q", secondPrompt)
	}
}

func TestLoadSharesProjectRuntimeAcrossEnvironments(t *testing.T) {
	cwd := mcpProject(t)
	first, notices := load(t, runner.Options{Cwd: cwd, TrustProjectMCP: true})
	if len(notices) != 0 {
		t.Fatalf("first load notices: %v", noticeTexts(notices))
	}
	second, notices := load(t, runner.Options{Cwd: cwd, TrustProjectMCP: true})
	if len(notices) != 0 {
		t.Fatalf("second load notices: %v", noticeTexts(notices))
	}
	if first.MCP != second.MCP {
		t.Fatal("same project created more than one MCP manager")
	}
	if first.Agents != second.Agents {
		t.Fatal("same project did not share its agent registry")
	}

	first.Close()
	reg, err := second.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("mcp__srv__echo")
	if !ok {
		t.Fatalf("shared MCP tool missing: %v", reg.Names())
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"text":"still alive"}`)); err != nil {
		t.Fatalf("first environment closed the shared MCP manager: %v", err)
	}
}

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
	env, _, err := runner.Load(context.Background(), runner.Options{Cwd: mcpProject(t), TrustProjectMCP: true})
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

// A prompt that carried no instructions must report none, or the session marks
// files as already-in-context that the model was never shown - and read_file
// answers a note instead of the contents.
func TestSystemPromptInjectsNothingWhenTheInstructionsAreEmpty(t *testing.T) {
	// The interesting case is not an absent file but a present, empty one: the
	// path exists, so only the prompt's own emptiness can say nothing was said.
	cwd := project(t)
	writeFile(t, filepath.Join(cwd, "AGENTS.md"), "")

	env, _ := load(t, runner.Options{Cwd: cwd})
	if _, injected := env.SystemPrompt(); len(injected) != 0 {
		t.Fatalf("an empty instruction file was reported as injected: %v", injected)
	}

	env, _ = load(t, runner.Options{Cwd: project(t)})
	if _, injected := env.SystemPrompt(); len(injected) != 0 {
		t.Fatalf("a project with no instruction files reported %v", injected)
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
