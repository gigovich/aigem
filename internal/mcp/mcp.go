package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gigovich/aigem/internal/tools"
)

// connectTimeout bounds how long a single server may take to handshake and
// enumerate before it is skipped (graceful degradation, like skills/hooks).
const connectTimeout = 15 * time.Second

// serverConn is one configured MCP server and, once connected, its session and
// enumerated capabilities.
type serverConn struct {
	name      string
	cfg       ServerConfig
	transport mcpsdk.Transport

	session   *mcpsdk.ClientSession
	tools     []*mcpsdk.Tool
	resources []*mcpsdk.Resource
	templates []*mcpsdk.ResourceTemplate
	prompts   []*mcpsdk.Prompt
	connected bool
	err       error // build or connection failure, surfaced in /mcp
}

// hasResources reports whether the server exposes any concrete resource or
// resource template (so the read tools are worth registering).
func (s *serverConn) hasResources() bool {
	return len(s.resources) > 0 || len(s.templates) > 0
}

// Manager owns the MCP client and one session per configured server.
type Manager struct {
	client  *mcpsdk.Client
	servers []*serverConn
	byName  map[string]*serverConn
}

// New builds a Manager from the MCP servers configured under cwd. It does not
// dial yet (call Connect). version labels this client to servers. Returned
// errors are non-fatal config warnings.
func New(cwd, version string) (*Manager, []error) {
	return NewWithTrust(cwd, version, false)
}

// NewWithTrust is like New, but trustProject explicitly approves project-local
// stdio MCP servers for the project before loading runtime configs.
func NewWithTrust(cwd, version string, trustProject bool) (*Manager, []error) {
	cfgs, warns := RuntimeConfigs(cwd, trustProject)
	m := newManager(version)
	for _, nc := range cfgs {
		if nc.cfg.Disabled {
			continue
		}
		t, err := transportFor(nc.name, nc.cfg)
		sc := &serverConn{name: nc.name, cfg: nc.cfg, transport: t, err: err}
		m.servers = append(m.servers, sc)
		m.byName[nc.name] = sc
	}
	return m, warns
}

func newManager(version string) *Manager {
	if version == "" {
		version = "0.0.0"
	}
	return &Manager{
		client: mcpsdk.NewClient(&mcpsdk.Implementation{Name: "aigem", Version: version}, nil),
		byName: map[string]*serverConn{},
	}
}

// addServer registers a pre-built transport (used by tests).
func (m *Manager) addServer(name string, cfg ServerConfig, t mcpsdk.Transport) {
	sc := &serverConn{name: name, cfg: cfg, transport: t}
	m.servers = append(m.servers, sc)
	m.byName[name] = sc
}

// Empty reports whether any MCP server is configured.
func (m *Manager) Empty() bool { return m == nil || len(m.servers) == 0 }

// transportFor infers the SDK transport from a server config.
func transportFor(name string, cfg ServerConfig) (mcpsdk.Transport, error) {
	switch {
	case cfg.URL != "":
		client := http.DefaultClient
		if len(cfg.Headers) > 0 {
			u, err := url.Parse(cfg.URL)
			if err != nil {
				return nil, fmt.Errorf("invalid url %q: %w", cfg.URL, err)
			}
			client = &http.Client{Transport: headerRoundTripper{
				headers: cfg.Headers, host: u.Host, base: http.DefaultTransport,
			}}
		}
		tr := &mcpsdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: client}
		if cfg.OAuth {
			tr.OAuthHandler = newOAuthHandler(name, cfg.URL)
		}
		return tr, nil
	case cfg.Command != "":
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Env = os.Environ()
		for k, v := range cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	default:
		return nil, fmt.Errorf("server config has neither a command (stdio) nor url (http)")
	}
}

// headerRoundTripper injects static headers (e.g. Authorization) on requests to
// the configured server host only. It withholds them on a cross-host redirect so
// credentials are never sent to a different origin.
type headerRoundTripper struct {
	headers map[string]string
	host    string // endpoint host the headers are scoped to
	base    http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != h.host {
		return h.base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// Connect dials every configured server concurrently, each bounded by
// connectTimeout, and enumerates its capabilities. A server that fails to
// connect is recorded and skipped; the session still starts.
func (m *Manager) Connect(ctx context.Context) {
	var wg sync.WaitGroup
	for _, sc := range m.servers {
		if sc.err != nil { // transport build already failed
			continue
		}
		wg.Add(1)
		go func(sc *serverConn) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			session, err := m.client.Connect(cctx, sc.transport, nil)
			if err != nil {
				sc.err = err
				return
			}
			sc.session = session
			sc.connected = true
			sc.enumerate(cctx)
		}(sc)
	}
	wg.Wait()
}

// enumerate loads the server's tools, resources, templates, and prompts. A
// capability the server does not support simply yields nothing.
func (s *serverConn) enumerate(ctx context.Context) {
	for t, err := range s.session.Tools(ctx, nil) {
		if err != nil {
			break
		}
		s.tools = append(s.tools, t)
	}
	for r, err := range s.session.Resources(ctx, nil) {
		if err != nil {
			break
		}
		s.resources = append(s.resources, r)
	}
	for t, err := range s.session.ResourceTemplates(ctx, nil) {
		if err != nil {
			break
		}
		s.templates = append(s.templates, t)
	}
	for p, err := range s.session.Prompts(ctx, nil) {
		if err != nil {
			break
		}
		s.prompts = append(s.prompts, p)
	}
}

// Warnings returns one line per server that failed to connect, for the TUI to
// surface like skill/hook errors.
func (m *Manager) Warnings() []string {
	var out []string
	for _, sc := range m.servers {
		if sc.err != nil {
			out = append(out, fmt.Sprintf("mcp server %q: %v", sc.name, sc.err))
		}
	}
	return out
}

// RegisterTools adds every connected server's tools (and resource read tools)
// to reg as MCP-origin tools, reachable by the main agent only.
func (m *Manager) RegisterTools(reg *tools.Registry) {
	for _, sc := range m.servers {
		if !sc.connected {
			continue
		}
		for _, t := range sc.tools {
			reg.RegisterMCP(newMCPTool(sc, t))
		}
		if sc.hasResources() {
			reg.RegisterMCP(newListResourcesTool(sc))
			reg.RegisterMCP(newReadResourceTool(sc))
		}
	}
}

// Prompt returns a short MCP summary for the system prompt (servers and their
// tool names), or "" when nothing connected.
func (m *Manager) Prompt() string {
	var b strings.Builder
	for _, sc := range m.servers {
		if !sc.connected {
			continue
		}
		names := make([]string, 0, len(sc.tools))
		for _, t := range sc.tools {
			names = append(names, toolName(sc.name, t.Name))
		}
		if sc.hasResources() {
			names = append(names, toolName(sc.name, "read_resource"))
		}
		if len(names) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", sc.name, strings.Join(names, ", "))
	}
	if b.Len() == 0 {
		return ""
	}
	return "# MCP servers\n\nThese Model Context Protocol servers are connected; call their tools " +
		"(named mcp__<server>__<tool>) when relevant:\n" + strings.TrimRight(b.String(), "\n")
}

// Close closes every open session. Safe to call on a nil Manager.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, sc := range m.servers {
		if sc.session != nil {
			_ = sc.session.Close()
		}
	}
}
