package web

import "net/http"

// features are the parts of the API this build serves. A page reads the map
// rather than the version string, so that a UI shipped ahead of a route hides
// the screen behind it instead of rendering one that 404s.
//
// Only what is true is in here, and an absent name means unsupported. Listing a
// route that does not exist yet as false would commit the wire to a vocabulary
// three stages before the code, with nothing to catch a flag that outlives its
// route or a name that gets spelled differently when it lands. Adding a line
// belongs in the commit that makes the feature real.
//
// The map belongs to one Server: tests and embedders may provide only a focused
// backend, and capability discovery must describe what that instance can serve.
//
// A type answers "was this built with the seam"; only the instance knows
// whether it was handed what the seam needs - a state directory it could
// create, a project that loaded - so a backend gets the last word through
// FeatureBackend. The routes stay reachable and answer for themselves - an
// empty collection, or 501 where there is nothing behind them at all; what this
// map decides is whether a page offers the screen, and offering one that can
// never hold anything is the failure it exists to prevent.
func featuresFor(b Backend) map[string]bool {
	out := map[string]bool{"controlSocket": true}
	if _, ok := b.(ActivityBackend); ok {
		out["activity"] = true
	}
	if _, ok := b.(CommandsBackend); ok {
		out["commands"] = true
	}
	if _, ok := b.(ModelsBackend); ok {
		out["models"] = true
	}
	if _, ok := b.(AuthBackend); ok {
		out["providerLogin"] = true
	}
	if _, ok := b.(RunsBackend); ok {
		out["runs"] = true
	}
	if _, ok := b.(SkillsBackend); ok {
		out["skills"] = true
	}
	if _, ok := b.(UsageBackend); ok {
		out["usage"] = true
	}
	if r, ok := b.(FeatureBackend); ok {
		for _, name := range r.Unavailable() {
			delete(out, name)
		}
	}
	return out
}

// FeatureBackend withdraws a capability this instance cannot serve. Its names
// are the ones featuresFor puts in the map; a name that is not in it is
// ignored, so an adapter may list a feature unconditionally.
type FeatureBackend interface {
	Unavailable() []string
}

// metaResponse is what /api/meta answers, and what hello carries. The two are
// the same document on purpose: hello is a re-base, and a page that has just
// reconnected must not have to make a second request to learn what it already
// got told.
//
// The backend's Meta is embedded rather than nested, so the wire stays flat and
// the fields the backend owns are named in one place. The names below are
// reserved by that: a Meta that grew a Rev, UI or Features field would lose it
// to the outer one silently, with nothing but the wire to say so.
type metaResponse struct {
	Meta
	// Rev is the revision this document is current as of. It is what makes the
	// snapshot the other half of the control stream's protocol: a page that
	// noticed a gap refetches and needs to know where the answer put it, or it
	// marks itself current at a revision the snapshot may predate.
	Rev uint64 `json:"rev"`
	// UI is whether this daemon was built with the browser bundle. It is the
	// Server's own answer and never the backend's: a daemon built without a UI
	// must not be able to claim it has one.
	UI       bool            `json:"ui"`
	Features map[string]bool `json:"features"`
}

func (s *Server) metaBody(rev uint64, m Meta) metaResponse {
	return metaResponse{Meta: m, Rev: rev, UI: s.hasUI, Features: s.features}
}

// handleMeta describes the daemon to a signed-in page.
//
// It is behind Guard, and the whole capability map with it. What is in here -
// the version, the model the operator has chosen, which features this build
// serves - is a description of the machine the daemon runs on, and there is no
// caller that needs it before signing in. /healthz is the one that answers
// without a credential, and it says nothing but that the process is up.
//
// The backend is asked on every request rather than cached: the default model
// changes while the daemon runs, because signing a provider in and picking a
// model are things this very UI does.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	// Before the snapshot, so the revision this document claims to be current
	// as of is one it cannot predate. See hub.current.
	rev := s.hub.current()
	meta, err := s.backend.Meta(r.Context())
	if err != nil {
		http.Error(w, "the daemon could not describe itself", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.metaBody(rev, meta))
}
