package web

import (
	"context"
	"errors"
)

// Backend is the agent side of the daemon, as the web package sees it.
//
// It is declared here and implemented in cmd/aigem rather than the other way
// round, so that internal/web never imports internal/runner - and with it
// uisession, tools, mcp, hooks, skill and llm. What the router serves is then
// testable against a fake, with no model, no MCP server and no agent behind it.
//
// A method here returns transport types this package can marshal and never a
// live object, and it names a model or a run the way the wire names it, because
// resolving that name is the backend's job and not the router's.
type Backend interface {
	// Meta describes the daemon to a signed-in page. It is the daemon's own
	// state rather than the request's, and it may change while the daemon runs,
	// so a page reads it again after anything that could have moved it.
	Meta(ctx context.Context) (Meta, error)
}

// Meta is what a page needs to know about the daemon it is talking to. The UI
// state is deliberately absent: which assets this Server carries is the
// Server's own answer, and asking the backend for it would let a daemon built
// without a UI claim to have one.
type Meta struct {
	// Version is the running binary's version, as `aigem version` prints it.
	Version string `json:"version"`
	// DefaultModel is the model reference a new session gets when none is
	// named, in the "provider/id" form the wire uses. Empty when nothing is
	// authenticated, which is a state the page has to be able to show.
	DefaultModel string `json:"defaultModel"`
}

// ErrNoBackend is returned by New when Config carries no Backend. A daemon
// without one could still serve the page, and the page would then fail one
// request at a time against a router with nothing behind it; refusing at
// construction reports the wiring mistake where it was made.
var ErrNoBackend = errors.New("web: no backend was given to serve the API with")
