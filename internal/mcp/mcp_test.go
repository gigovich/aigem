package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gigovich/aigem/internal/tools"
	projecttrust "github.com/gigovich/aigem/internal/trust"
)

// testServer builds an in-process MCP server exposing one default-confirm tool,
// one read-only tool, a resource, and a prompt, and returns a connected Manager.
func testServer(t *testing.T) *Manager {
	t.Helper()
	ctx := context.Background()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "v1"}, nil)

	type echoIn struct {
		Text string `json:"text"`
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo back text"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
		})
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "peek", Description: "read-only peek",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "peeked"}}}, nil, nil
	})

	srv.AddResource(&mcpsdk.Resource{URI: "test://greeting", Name: "greeting", Description: "a greeting"},
		func(_ context.Context, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{
				Contents: []*mcpsdk.ResourceContents{{URI: "test://greeting", Text: "hello there"}},
			}, nil
		})

	srv.AddPrompt(&mcpsdk.Prompt{
		Name: "greet", Description: "greet someone",
		Arguments: []*mcpsdk.PromptArgument{{Name: "who", Required: true}},
	}, func(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
		return &mcpsdk.GetPromptResult{
			Messages: []*mcpsdk.PromptMessage{
				{Role: "user", Content: &mcpsdk.TextContent{Text: "Say hi to " + req.Params.Arguments["who"]}},
			},
		}, nil
	})

	st, ct := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	m := newManager("test")
	m.addServer("srv", ServerConfig{}, ct)
	m.Connect(ctx)
	t.Cleanup(m.Close)
	return m
}

func TestToolDiscoveryAndDispatch(t *testing.T) {
	m := testServer(t)
	reg, err := tools.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.RegisterTools(reg)

	if _, ok := reg.Get("mcp__srv__echo"); !ok {
		t.Fatal("mcp__srv__echo not registered")
	}
	echo, _ := reg.Get("mcp__srv__echo")
	out, err := echo.Run(context.Background(), json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("echo run: %v", err)
	}
	if out != "hi" {
		t.Fatalf("echo = %q, want hi", out)
	}
}

func TestConfirmGate(t *testing.T) {
	m := testServer(t)
	reg, _ := tools.NewRegistry(t.TempDir())
	m.RegisterTools(reg)

	echo, _ := reg.Get("mcp__srv__echo")
	if !echo.NeedsConfirm() {
		t.Error("echo (no annotation) should need confirmation")
	}
	peek, _ := reg.Get("mcp__srv__peek")
	if peek.NeedsConfirm() {
		t.Error("peek (readOnlyHint) should not need confirmation")
	}
}

func TestAutoApproveSkipsConfirm(t *testing.T) {
	ann := &mcpsdk.ToolAnnotations{}
	if confirmFor(ann, []string{"do_it"}, "do_it") {
		t.Error("auto-approved tool should not need confirmation")
	}
	if !confirmFor(ann, []string{"other"}, "do_it") {
		t.Error("non-listed tool should need confirmation")
	}
}

func TestDestructiveOverridesAutoApprove(t *testing.T) {
	yes := true
	ann := &mcpsdk.ToolAnnotations{DestructiveHint: &yes}
	if !confirmFor(ann, []string{"nuke"}, "nuke") {
		t.Error("a destructive tool must confirm even when auto-approved")
	}
}

func TestHeaderRoundTripperScopesToHost(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	host := srv.URL[strings.LastIndex(srv.URL, "/")+1:]

	rt := headerRoundTripper{headers: map[string]string{"Authorization": "Bearer secret"}, host: host, base: http.DefaultTransport}
	client := &http.Client{Transport: rt}

	// Same host: header is injected.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sawAuth != "Bearer secret" {
		t.Fatalf("same-host auth = %q, want injected", sawAuth)
	}

	// Header scoped to a different host: withheld when talking to this server.
	otherRT := headerRoundTripper{headers: rt.headers, host: "evil.example.com:1", base: http.DefaultTransport}
	sawAuth = ""
	resp, err = (&http.Client{Transport: otherRT}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sawAuth != "" {
		t.Fatalf("cross-host auth leaked: %q", sawAuth)
	}
}

func TestSubagentsExcludeMCP(t *testing.T) {
	m := testServer(t)
	reg, _ := tools.NewRegistry(t.TempDir())
	m.RegisterTools(reg)

	sub := reg.Subset(reg.Names()) // even asking for everything drops MCP tools
	for _, n := range sub.Names() {
		if strings.HasPrefix(n, "mcp__") {
			t.Fatalf("subset leaked MCP tool %q to subagents", n)
		}
	}
	if _, ok := sub.Get("mcp__srv__echo"); ok {
		t.Fatal("subagent subset must not contain MCP tools")
	}
}

func TestPromptToTurn(t *testing.T) {
	m := testServer(t)
	cmds := m.PromptCommands()
	if len(cmds) != 1 || cmds[0].Name != "mcp__srv__greet" {
		t.Fatalf("prompt commands = %+v", cmds)
	}
	if len(cmds[0].Args) != 1 || cmds[0].Args[0] != "who" {
		t.Fatalf("prompt args = %+v", cmds[0].Args)
	}
	body, err := m.RenderPrompt(context.Background(), "mcp__srv__greet", "world")
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	if body != "Say hi to world" {
		t.Fatalf("prompt body = %q", body)
	}
}

func TestResourceListAndRead(t *testing.T) {
	m := testServer(t)
	reg, _ := tools.NewRegistry(t.TempDir())
	m.RegisterTools(reg)

	list, ok := reg.Get("mcp__srv__list_resources")
	if !ok {
		t.Fatal("list_resources not registered")
	}
	out, err := list.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("list run: %v", err)
	}
	if !strings.Contains(out, "test://greeting") {
		t.Fatalf("list output = %q", out)
	}

	read, _ := reg.Get("mcp__srv__read_resource")
	got, err := read.Run(context.Background(), json.RawMessage(`{"uri":"test://greeting"}`))
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got != "hello there" {
		t.Fatalf("read output = %q", got)
	}

	// Human-facing preview path.
	prev, err := m.ReadResource(context.Background(), "srv", "test://greeting")
	if err != nil || prev != "hello there" {
		t.Fatalf("ReadResource = %q, %v", prev, err)
	}
}

func TestGracefulDegradation(t *testing.T) {
	m := newManager("test")
	cmd := ServerConfig{Command: "aigem-no-such-binary-xyz"}
	tr, err := transportFor("broken", cmd)
	if err != nil {
		t.Fatalf("transport build: %v", err)
	}
	m.addServer("broken", cmd, tr)
	m.Connect(context.Background())

	if len(m.Warnings()) == 0 {
		t.Fatal("expected a warning for the failed server")
	}
	reg, _ := tools.NewRegistry(t.TempDir())
	m.RegisterTools(reg) // must not panic or register anything
	for _, n := range reg.Names() {
		if strings.HasPrefix(n, "mcp__") {
			t.Fatalf("failed server registered tool %q", n)
		}
	}
}

func TestLoadConfigsPrecedence(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project.json")
	global := filepath.Join(dir, "global.json")
	os.WriteFile(project, []byte(`{"mcpServers":{"fs":{"command":"proj"}}}`), 0o644)
	os.WriteFile(global, []byte(`{"mcpServers":{"fs":{"command":"glob"},"extra":{"url":"http://x"}}}`), 0o644)

	cfgs, warns := loadConfigs([]string{project, global})
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	byName := map[string]ServerConfig{}
	for _, nc := range cfgs {
		byName[nc.name] = nc.cfg
	}
	if byName["fs"].Command != "proj" {
		t.Fatalf("project should win: got %q", byName["fs"].Command)
	}
	if byName["extra"].URL != "http://x" {
		t.Fatalf("global-only server missing: %+v", byName["extra"])
	}
}

func TestMalformedConfigWarns(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{not json`), 0o644)
	_, warns := loadConfigs([]string{bad})
	if len(warns) != 1 {
		t.Fatalf("want 1 warning, got %v", warns)
	}
}

func TestRuntimeConfigsKeepTransportCapabilitiesSeparate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"local":{"command":"echo"},"remote":{"url":"https://example.com/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := mcpTransportFingerprint(ServerConfig{Command: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := projecttrust.Approve(root, projecttrust.CapabilityMCPStdio, "local", fp, "test"); err != nil {
		t.Fatal(err)
	}
	cfgs, warns := RuntimeConfigs(root, false)
	if len(cfgs) != 1 || cfgs[0].name != "local" {
		t.Fatalf("stdio approval widened to HTTP: cfgs=%+v warnings=%v", cfgs, warns)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Error(), `http MCP server "remote" is pending`) {
		t.Fatalf("HTTP pending warning = %v", warns)
	}
}

func TestRuntimeConfigsInvalidatesOnlyChangedApprovalPolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"remote":{"url":"https://example.com/mcp","autoApprove":["read"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs, warns := RuntimeConfigs(root, true)
	if len(warns) != 0 || len(cfgs) != 1 || len(cfgs[0].cfg.AutoApprove) != 1 {
		t.Fatalf("initial approval: cfgs=%+v warnings=%v", cfgs, warns)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"remote":{"url":"https://example.com/mcp","autoApprove":["write"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs, warns = RuntimeConfigs(root, false)
	if len(cfgs) != 1 {
		t.Fatalf("policy change disabled approved transport: cfgs=%+v warnings=%v", cfgs, warns)
	}
	if len(cfgs[0].cfg.AutoApprove) != 0 {
		t.Fatalf("invalidated policy remained active: %+v", cfgs[0].cfg.AutoApprove)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Error(), "policy") || !strings.Contains(warns[0].Error(), "invalidated") {
		t.Fatalf("policy invalidation warning = %v", warns)
	}
}

func TestRuntimeConfigsGateProjectLocalMCP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"local":{"command":"echo"},"remote":{"url":"http://example.com/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgs, warns := RuntimeConfigs(root, false)
	if len(cfgs) != 0 {
		t.Fatalf("untrusted project MCP configs = %+v, want none", cfgs)
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v, want untrusted stdio and http warnings", warns)
	}
	joined := warns[0].Error() + "\n" + warns[1].Error()
	if !strings.Contains(joined, "project-local stdio MCP server") || !strings.Contains(joined, "project-local http MCP server") {
		t.Fatalf("warnings = %v, want untrusted stdio and http warnings", warns)
	}

	cfgs, warns = RuntimeConfigs(root, true)
	if len(warns) != 0 {
		t.Fatalf("trusted warnings = %v", warns)
	}
	byName := map[string]ServerConfig{}
	for _, nc := range cfgs {
		byName[nc.name] = nc.cfg
	}
	if byName["local"].Command != "echo" || byName["remote"].URL == "" {
		t.Fatalf("trusted configs missing project servers: %+v", byName)
	}

	cfgs, warns = RuntimeConfigs(root, false)
	if len(warns) != 0 || len(cfgs) != 2 {
		t.Fatalf("persisted trust configs=%+v warnings=%v, want both servers and no warnings", cfgs, warns)
	}
}
