package runner

import (
	"errors"
	"fmt"

	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/uisession"
)

// SkillApproval is what approving a project's skill definitions produced.
type SkillApproval struct {
	// Loaded names the project-local skills that are now available, sanitized
	// for display. It is what a person should be told about rather than the
	// names that were pending: approval re-fingerprints the files, so an edit
	// made in between can leave the set smaller than the question implied.
	Loaded []string
	// Notices are the definitions discovery skipped, one per broken file. They
	// are not errors: the rest of the project's skills loaded.
	Notices []Notice
	// Catalog is the registry the sessions now read. It is the one that was
	// passed in - replaced in place - unless that was nil, in which case it is
	// the freshly discovered set and the caller has to keep it.
	Catalog *skill.Registry
}

// ApproveSkills approves the project-local skill definitions under cwd as they
// read now, re-runs discovery once, and brings every session to the result.
//
// Discovery runs once per approval rather than once per session: the sessions
// of one project share a catalog, and scanning per session would both repeat
// the work and let two of them describe different sets of skills.
//
// It refuses before approving anything if any session is mid-turn: the change
// re-registers a tool whose schema that turn is already using. A session that
// starts a turn during the update keeps the catalog it had - the returned error
// names it, the approval itself stands, and repeating the call once the turn
// ends brings it into line.
func ApproveSkills(cwd string, catalog *skill.Registry, sessions ...*Session) (SkillApproval, error) {
	for _, s := range sessions {
		if s != nil && s.Local != nil && s.Local.Running() {
			return SkillApproval{Catalog: catalog}, fmt.Errorf(
				"runner: approving skills would change the tools a running turn is using: %w",
				uisession.ErrBusy)
		}
	}
	if err := skill.ApproveProject(cwd); err != nil {
		return SkillApproval{Catalog: catalog}, fmt.Errorf("runner: approve project skills in %s: %w", cwd, err)
	}
	found, errs := skill.Discover(cwd)
	out := SkillApproval{Catalog: catalog}
	for _, err := range errs {
		out.Notices = append(out.Notices, Notice{Text: "skipped skill: " + err.Error(), InChat: true})
	}
	if out.Catalog == nil {
		out.Catalog = found
	} else {
		// Replaced in place: the skill tool and the system-prompt builder hold
		// this pointer, and handing them a new registry would leave them
		// describing the set that was there before the approval.
		out.Catalog.Replace(found)
	}
	for _, s := range out.Catalog.List() {
		if s.ProjectLocal {
			out.Loaded = append(out.Loaded, skill.DisplaySafe(s.Name))
		}
	}

	var failed []error
	for _, s := range sessions {
		if s == nil {
			continue
		}
		// A session closed while it was still attached is gone, not a failure:
		// there is nothing left to tell about the new catalog.
		if err := s.SetSkills(out.Catalog); err != nil && !errors.Is(err, uisession.ErrClosed) {
			failed = append(failed, err)
		}
	}
	return out, errors.Join(failed...)
}

// Attach registers a session so that changes made to this environment reach it.
// Approving the project's skills is the only one today.
//
// The caller detaches a session it closes; an attached session that is closed
// without being detached is skipped rather than reported, but is held onto
// until the environment goes.
func (e *Env) Attach(s *Session) {
	if e == nil || s == nil {
		return
	}
	e.sessMu.Lock()
	defer e.sessMu.Unlock()
	for _, have := range e.sessions {
		if have == s {
			return
		}
	}
	e.sessions = append(e.sessions, s)
}

// Detach undoes Attach. Detaching a session that was never attached is a no-op.
func (e *Env) Detach(s *Session) {
	if e == nil || s == nil {
		return
	}
	e.sessMu.Lock()
	defer e.sessMu.Unlock()
	for i, have := range e.sessions {
		if have == s {
			e.sessions = append(e.sessions[:i], e.sessions[i+1:]...)
			return
		}
	}
}

// ApproveProjectSkills approves this project's skill definitions and brings
// every attached session to the result: the environment's catalog, the skill
// tool each session offers and each session's system prompt all describe the
// same set when it returns.
//
// It writes the environment's own Pending and Skills, so it belongs to whoever
// owns the Env - the front-end or the daemon that loaded it - rather than to a
// session goroutine. The per-session updates are each taken under that
// session's own lock.
func (e *Env) ApproveProjectSkills() (SkillApproval, error) {
	if e == nil {
		return SkillApproval{}, errors.New("runner: no environment to approve skills in")
	}
	if e.closed.Load() {
		return SkillApproval{}, errors.New("runner: the environment is closed")
	}
	e.sessMu.Lock()
	attached := append([]*Session(nil), e.sessions...)
	e.sessMu.Unlock()

	res, err := ApproveSkills(e.Cwd, e.Skills, attached...)
	if res.Catalog != nil {
		e.Skills = res.Catalog
	}
	if err == nil {
		// Discovery has just been re-run against the approved fingerprints, so
		// whatever it withheld before is either loaded or reported as skipped.
		e.Pending = nil
	}
	return res, err
}
