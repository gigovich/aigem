package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
	"github.com/gigovich/aigem/internal/web"
)

const webUsage = `usage:
  aigem web run                       serve the web UI and the session protocol
  aigem -listen host:port web run     bind somewhere other than a random loopback port
  aigem -listen 0.0.0.0:7777 -origin https://aigem.example.ts.net web run

Flags come before "web run": they are the global set, and the command is what
follows them.

Runs on 127.0.0.1 by default. The URL printed at startup carries a token; the
browser trades it once for a cookie, and the CLI keeps using it. Serving on
anything but loopback needs -origin, the public URL a reverse proxy reaches this
daemon at - see docs/bots.md.
`

// webRun is everything the daemon needs to build sessions, assembled by the
// normal startup path so a web session is put together exactly like a terminal
// one.
type webRun struct {
	client *llm.Ref
	// newRegistry builds a sandbox per conversation. Sessions cannot share one:
	// the delegation and skill tools carry the confirmation function of whichever
	// session registered them last.
	newRegistry func() (*tools.Registry, error)
	temp        float64
	sysPrompt   string
	buildSys    func() string
	agents      *agent.SubagentRegistry
	project     string
	skills      *skill.Registry
	hooks       *hooks.Runner
	mcpMgr      *mcp.Manager
	compactCfg  agent.CompactConfig
	modelReg    *llm.Registry
	maxTokens   int
	ctxSize     int
	addr        string
	origins     []string
	cwd         string
}

// runWeb starts the daemon and serves until interrupted.
func runWeb(o webRun) {
	// Checked before binding: failing after taking a port would be a second
	// daemon's footprint on a machine that already has one.
	if prior, running, err := web.LoadState(); err == nil && running {
		fatal(fmt.Errorf("a daemon is already running on %s (pid %d); stop it first",
			prior.Addr, prior.PID))
	}
	// A state directory this daemon cannot have is not worth refusing to serve
	// over: it costs the browser sessions across a restart, and nothing else.
	var cookieFile string
	if dir, err := config.StateDir(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not find the state directory:", err)
	} else {
		cookieFile = filepath.Join(dir, "web-cookies.json")
	}
	srv, err := web.New(web.Config{
		Addr:       o.addr,
		Origins:    o.origins,
		Factory:    o.factory(),
		Assets:     web.Assets(),
		Models:     o.modelReg,
		Backend:    o.client,
		CookieFile: cookieFile,
	})
	if err != nil {
		fatal(err)
	}
	if err := web.SaveState(web.State{
		PID: os.Getpid(), Addr: srv.Addr().String(), Token: srv.Token(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not record the daemon:", err)
	}
	defer func() { _ = web.ClearState() }()

	fmt.Println("aigem web is serving " + o.cwd)
	fmt.Println("  " + srv.URL())
	if !web.HasAssets() {
		fmt.Fprintln(os.Stderr,
			"note: this build has no web UI (built without `make web`); the API is up, the page is not.")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()

	select {
	case <-stop:
		fmt.Println("\nstopping; saving sessions")
	case err := <-errc:
		if err != nil {
			_ = srv.Close()
			fatal(err)
		}
	}
	// Close saves every session before ending it, so a turn interrupted by the
	// shutdown is still resumable.
	if err := srv.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	o.mcpMgr.Close()
}

// factory builds one session per conversation, each with its own sandbox.
// What is still shared is what was resolved for this root at startup - skills,
// project instructions, trust - which is what confines the daemon to the
// directory it was started in: another root means resolving those again rather
// than reusing these.
func (o webRun) factory() web.Factory {
	return func(spec web.Spec) (*uisession.Local, error) {
		if spec.Cwd != "" && spec.Cwd != o.cwd {
			return nil, errors.New("this daemon serves " + o.cwd +
				"; a session in another directory needs its own daemon for now")
		}
		reg, err := o.newRegistry()
		if err != nil {
			return nil, err
		}
		if o.mcpMgr != nil && !o.mcpMgr.Empty() {
			o.mcpMgr.RegisterTools(reg)
		}
		// The retry notice needs the session it belongs to, which does not exist
		// until the stream it wraps has been built. Capturing it keeps a
		// provider hiccup visible in the conversation instead of nowhere.
		var sess *uisession.Local
		stream := retrying(o.client, func(text string) {
			if sess != nil {
				sess.Notice(text)
			}
		})
		sess = uisession.New(uisession.Config{
			Tools: reg,
			NewAgent: func(confirm agent.ConfirmFunc) *agent.Agent {
				if o.agents != nil {
					reg.Register(agent.NewTaskTool(stream, reg, o.temp, confirm, o.agents, o.project))
				}
				registerSkillTool(reg, o.skills, stream, o.temp, confirm)
				ag := agent.New(stream, reg, o.temp, confirm, o.sysPrompt)
				reg.Register(agent.NewTodoTool(ag))
				ag.SetHooks(o.hooks)
				ag.SetCompaction(o.compactCfg)
				if o.skills != nil {
					ag.WatchSkills(o.skills.Conditional())
				}
				return ag
			},
			Hooks:         o.hooks,
			ModelRef:      func() string { return o.client.Model().Ref() },
			RebuildSystem: o.buildSys,
			Models:        o.modelReg,
			Backend:       o.client,
			MaxTokens:     o.maxTokens,
			CtxSize:       o.ctxSize,
			Compact:       o.compactCfg,
		})
		return sess, nil
	}
}
