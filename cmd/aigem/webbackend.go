package main

import (
	"context"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/web"
)

// webBackend is the wiring internal/web is deliberately unable to do itself:
// the web package declares what it needs and never imports internal/runner or
// internal/llm, so the binary is the one place that knows how a model
// reference is resolved and what this build is called.
type webBackend struct {
	version string
	// models is the configured providers, read once: the file behind it is the
	// operator's, not the daemon's, and a change to it is a restart either way.
	models *llm.Registry
}

func newWebBackend(version string) *webBackend {
	return &webBackend{version: version, models: defaultModelRegistry()}
}

// Meta reads the saved preference and the credential store on every call
// rather than caching the answer. Both change while the daemon runs - signing a
// provider in and picking a model are things the UI itself does - and a cached
// default model would go on naming a provider the operator has just signed out
// of. It is a couple of small file reads, on a route a page asks for once.
func (b *webBackend) Meta(_ context.Context) (web.Meta, error) {
	return web.Meta{
		Version:      b.version,
		DefaultModel: preferredModelRef(b.models),
	}, nil
}
