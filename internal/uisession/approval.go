package uisession

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/gigovich/aigem/internal/pathgrant"
	"github.com/gigovich/aigem/internal/tools"
)

// ErrAlreadyDecided is returned by Resolve for a request that is no longer
// open. With several front-ends attached this is the normal outcome of two
// people answering at once, not a failure: the first answer stands and the
// others learn who gave it from the approval_resolved event.
var ErrAlreadyDecided = errors.New("approval already decided")

// ErrBadDecision is returned when a decision is not one the request offered -
// an "always" on a write outside the working directory, say, which is a button
// that would not do what it says.
var ErrBadDecision = errors.New("decision not offered for this request")

// Decision is an answer to an approval request. The last option a request
// offers is always the refusal, so a front-end can bind Esc without knowing
// which dialog is showing.
type Decision string

const (
	DecisionOnce   Decision = "once"
	DecisionAlways Decision = "always"
	DecisionDeny   Decision = "deny"
)

// ApprovalKind distinguishes the two questions that share the dialog: "run this
// tool?" and "reach outside the working directory?".
type ApprovalKind string

const (
	ApprovalTool ApprovalKind = "tool"
	ApprovalPath ApprovalKind = "path"
)

// Option is one answer a request offers, with the label to show for it. The
// labels live here rather than in each front-end because they are not
// decoration: "Always" means something different for a tool, for a read outside
// the root, and for a write outside the root.
type Option struct {
	Value Decision `json:"value"`
	Label string   `json:"label"`
}

// Approval describes an open request.
type Approval struct {
	Kind    ApprovalKind    `json:"kind"`
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args,omitempty"`
	Path    string          `json:"path,omitempty"`
	Write   bool            `json:"write,omitempty"`
	Options []Option        `json:"options"`
}

func toolOptions() []Option {
	return []Option{
		{DecisionOnce, "Once"},
		{DecisionAlways, "Always"},
		{DecisionDeny, "Forbid"},
	}
}

func pathOptions(write bool) []Option {
	if write {
		// A write outside the working directory is never remembered, so offering
		// "Always" here would promise something the sandbox refuses to do.
		return []Option{{DecisionOnce, "Once"}, {DecisionDeny, "Deny"}}
	}
	return []Option{
		{DecisionOnce, "Once"},
		{DecisionAlways, "Always (this folder)"},
		{DecisionDeny, "Deny"},
	}
}

func (a Approval) offers(d Decision) bool {
	for _, o := range a.Options {
		if o.Value == d {
			return true
		}
	}
	return false
}

// pending is one request waiting for an answer, with the channel the blocked
// tool call is parked on. Exactly one of tool/path is non-nil.
type pending struct {
	id  string
	req Approval

	tool chan bool
	path chan tools.PathDecision
}

// answer unparks the blocked call and reports a notice worth showing, which
// only an "Always" on a path produces: it grants the whole directory, here and
// in later sessions, and that is worth saying out loud.
func (p *pending) answer(d Decision) (notice string) {
	if p.tool != nil {
		p.tool <- d != DecisionDeny
		return ""
	}
	switch d {
	case DecisionDeny:
		p.path <- tools.PathDeny
	case DecisionOnce:
		p.path <- tools.PathAllowOnce
	default:
		p.path <- tools.PathAllowDir
		return "allowed " + filepath.Dir(p.req.Path) + " for this project"
	}
	return ""
}

// confirmTool is the agent's ConfirmFunc. It runs on the tool call's own
// goroutine and blocks it until someone answers.
func (l *Local) confirmTool(name string, args json.RawMessage) bool {
	l.mu.Lock()
	// A session policy set by an earlier "Always"/"Forbid" answers immediately,
	// and so does auto mode for anything the code can undo. A destructive
	// command still asks.
	switch l.toolPolicy[name] {
	case policyAllow:
		l.mu.Unlock()
		return true
	case policyDeny:
		l.mu.Unlock()
		return false
	}
	if l.autoMode && !tools.IsDestructive(name, args) {
		l.mu.Unlock()
		return true
	}
	p := &pending{
		id:   l.nextApprovalID(),
		req:  Approval{Kind: ApprovalTool, Tool: name, Args: args, Options: toolOptions()},
		tool: make(chan bool, 1),
	}
	l.enqueueLocked(p)
	l.mu.Unlock()

	select {
	case ok := <-p.tool:
		return ok
	case <-l.done:
		return false
	}
}

// approvePath is the tool registry's PathApprover. Unlike a tool confirmation
// it is never settled by a session policy or by auto mode: those govern which
// tools may run, and leaving the working directory is a separate question that
// only a human answers.
func (l *Local) approvePath(path string, intent tools.PathIntent) tools.PathDecision {
	l.mu.Lock()
	p := &pending{
		id: l.nextApprovalID(),
		req: Approval{
			Kind: ApprovalPath, Tool: intent.Tool, Path: path,
			Write: intent.Write, Options: pathOptions(intent.Write),
		},
		path: make(chan tools.PathDecision, 1),
	}
	l.enqueueLocked(p)
	l.mu.Unlock()

	select {
	case d := <-p.path:
		return d
	case <-l.done:
		return tools.PathDeny
	}
}

// enqueueLocked makes p the open request, or parks it behind the one already
// open. Concurrent subagents can ask at once; one question is shown at a time
// and the rest wait, which is also why a queued request is not announced until
// it becomes the open one.
func (l *Local) enqueueLocked(p *pending) {
	if l.active != nil {
		l.queue = append(l.queue, p)
		return
	}
	l.openLocked(p)
}

func (l *Local) openLocked(p *pending) {
	l.active = p
	req := p.req
	l.emitLocked(Event{Kind: KindApprovalRequest, ID: p.id, Approval: &req})
}

// Resolve answers the open request. The first answer wins: a second one for the
// same id gets ErrAlreadyDecided, because by then the id names a request that
// no longer exists. Every subscriber sees the resolution, so a front-end whose
// answer lost the race closes its dialog knowing who won rather than showing an
// error.
func (l *Local) Resolve(id string, d Decision, by string) error {
	l.mu.Lock()
	p := l.active
	if p == nil || p.id != id {
		l.mu.Unlock()
		return ErrAlreadyDecided
	}
	if !p.req.offers(d) {
		l.mu.Unlock()
		return ErrBadDecision
	}
	if p.req.Kind == ApprovalTool {
		switch d {
		case DecisionAlways:
			l.toolPolicy[p.req.Tool] = policyAllow
		case DecisionDeny:
			l.toolPolicy[p.req.Tool] = policyDeny
		}
	}
	l.active = nil
	notice := p.answer(d)
	l.emitLocked(Event{Kind: KindApprovalResolved, ID: id, Decision: d, By: by})
	if notice != "" {
		l.emitLocked(Event{Kind: KindNotice, Text: notice})
	}
	l.promoteLocked()
	l.mu.Unlock()
	return nil
}

// promoteLocked opens the next queued request, settling on the way any that the
// answer just given already covers. Asking twice for the same tool, or for a
// directory the user just granted, is exactly what "Always" was meant to
// prevent.
func (l *Local) promoteLocked() {
	for len(l.queue) > 0 {
		next := l.queue[0]
		l.queue = l.queue[1:]
		if next.path != nil {
			if l.pathGranted(next.req) {
				next.path <- tools.PathAllowOnce
				continue
			}
			l.openLocked(next)
			return
		}
		switch l.toolPolicy[next.req.Tool] {
		case policyAllow:
			next.tool <- true
		case policyDeny:
			next.tool <- false
		default:
			if l.autoMode && !tools.IsDestructive(next.req.Tool, next.req.Args) {
				next.tool <- true
				continue
			}
			l.openLocked(next)
			return
		}
	}
}

// pathGranted reports whether a queued path request is already covered by a
// persisted grant - the user may have just approved a directory above it.
func (l *Local) pathGranted(req Approval) bool {
	if req.Write || l.tools == nil {
		return false // a write is never covered by a grant
	}
	ok, err := pathgrant.Allowed(l.tools.Root(), req.Path)
	return err == nil && ok
}

// Pending returns the open approval request, or nil. A front-end that attaches
// mid-turn needs it: the request event was emitted before it subscribed, and
// the turn is blocked until someone answers.
func (l *Local) Pending() (string, *Approval) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == nil {
		return "", nil
	}
	req := l.active.req
	return l.active.id, &req
}

// failPendingLocked refuses every outstanding request, used when the session
// closes so no tool call is left parked forever.
func (l *Local) failPendingLocked() {
	all := l.queue
	l.queue = nil
	if l.active != nil {
		all = append([]*pending{l.active}, all...)
		l.active = nil
	}
	for _, p := range all {
		if p.tool != nil {
			p.tool <- false
			continue
		}
		p.path <- tools.PathDeny
	}
}
