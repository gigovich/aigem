package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/uisession"
	"github.com/gigovich/aigem/internal/web"
)

// The daemon's half of the phase-one API: everything internal/web declares and
// deliberately cannot do itself, because doing it means knowing how a model
// reference resolves, where credentials live and what a project's skills are.
//
// Nothing here hands back a live object. Each method answers with the transport
// types internal/web owns, so the seam stays one that a second front-end could
// implement.

func (b *webBackend) Models(context.Context) ([]web.Model, error) {
	def := preferredModelRef(b.models)
	infos := b.models.Models()
	// One call rather than one per model: the answer differs per model, because
	// an OAuth subscription covers some of a provider's models and not others,
	// but the stored credential does not - and asking per model is a locked
	// parse of the whole credential file each time. How many models there are is
	// not this daemon's choice: a project's own models.json adds to the
	// registry, so per-model reads are a cost a repository could pick.
	usable := auth.UsableModels(b.models, defaultMaxTokens)
	out := make([]web.Model, 0, len(infos))
	for _, info := range infos {
		p, _, err := b.models.Resolve(info.Ref())
		if err != nil {
			continue
		}
		out = append(out, web.Model{
			Ref: info.Ref(), Provider: p.ID, Name: info.Name,
			ContextWindow: info.ContextWindow, MaxTokens: info.MaxTokens, Reasoning: info.Reasoning,
			NeedsAuth: p.NeedsAuth(), Authenticated: usable[info.Ref()],
			Default: info.Ref() == def,
		})
	}
	return out, nil
}

func (b *webBackend) SetDefaultModel(_ context.Context, ref string) (web.Model, error) {
	// The reference is the client's, so the reason it cannot be resolved is
	// written here rather than passed through: the resolver's own errors name
	// providers and, through the credential store, absolute paths.
	p, info, err := b.models.Resolve(ref)
	if err != nil {
		return web.Model{}, web.Refuse(fmt.Errorf("no model called %q is configured", ref))
	}
	if _, _, _, err := auth.OpenModel(b.models, info.Ref(), defaultMaxTokens); err != nil {
		slog.Warn("a model could not be opened", "ref", info.Ref(), "err", err)
		return web.Model{}, web.Refuse(fmt.Errorf(
			"%s is not signed in; sign in to %s first", info.Ref(), p.ID))
	}
	ref = info.Ref()
	out := web.Model{
		Ref: ref, Provider: p.ID, Name: info.Name, ContextWindow: info.ContextWindow,
		MaxTokens: info.MaxTokens, Reasoning: info.Reasoning, NeedsAuth: p.NeedsAuth(),
		Authenticated: true, Default: true,
	}
	// Setting the default to what is already *saved* changes nothing, and must
	// not look as though it did: every repeat would otherwise be a durable
	// write, a line in the activity feed and a message to every open tab, which
	// is a state directory a client can grow by holding down a button. The
	// comparison is against the saved preference and not against the effective
	// default, because "nothing is saved" is a state a person is entitled to
	// leave by naming the model that was being defaulted to anyway.
	if config.LoadPrefs().Model == ref {
		return out, nil
	}
	if err := config.SaveModelPref(ref); err != nil {
		return web.Model{}, err
	}
	b.recordActivity(web.Activity{Kind: "model.default", Text: "Default model changed to " + ref})
	b.publish("model.default", out)
	return out, nil
}

const (
	// loginStartTimeout bounds only the provider call that opens a login, not
	// the login: a person has minutes to authorize one, but the request that
	// starts it must not hold a handler goroutine for that long.
	loginStartTimeout             = 20 * time.Second
	maxPendingLogins              = 8
	maxPendingLoginsPerProvider   = 4
	maxRetainedTerminalLoginFlows = 32
	// pendingSkillsTTL bounds how often the skill listing reads the project.
	// Short enough that a skill added by hand appears while the person is still
	// looking at the screen, long enough that a polling page does not pay for
	// the whole tree on every request.
	pendingSkillsTTL = 2 * time.Second
)

// pendingLoginsFor is how many logins a provider can have in flight at once.
//
// It is not one number, because it is not one mechanism. A device-code flow
// polls the provider and can run several times over; the ChatGPT flow redirects
// to a fixed loopback port and there is one of those, so a second concurrent
// one cannot succeed however generous the cap is. Letting it through anyway
// produced a bind failure dressed up as a daemon fault.
func pendingLoginsFor(provider string) int {
	if provider == llm.OpenAIProviderID {
		return 1
	}
	return maxPendingLoginsPerProvider
}

// startedLogin is what the goroutine below hands back: the flow, or why there
// is not one, plus the provider whose reservation it holds.
type startedLogin struct {
	provider string
	f        *auth.Flow
	err      error
}

func (b *webBackend) BeginLogin(ctx context.Context, req web.LoginRequest) (web.Login, error) {
	if req.Provider != llm.OpenAIProviderID && req.Provider != llm.XAIProviderID {
		return web.Login{}, web.Refuse(fmt.Errorf("provider %s has no browser login", req.Provider))
	}
	b.flowMu.Lock()
	if b.closed {
		b.flowMu.Unlock()
		return web.Login{}, errors.New("the web backend is closed")
	}
	global, provider := b.flowStarting[""], b.flowStarting[req.Provider]
	for _, f := range b.flows {
		if state, _ := f.Status(); state == auth.FlowPending {
			global++
			if f.Provider == req.Provider {
				provider++
			}
		}
	}
	if global >= maxPendingLogins || provider >= pendingLoginsFor(req.Provider) {
		b.flowMu.Unlock()
		return web.Login{}, web.Busy("a sign-in to " + req.Provider + " is already in progress")
	}
	// Reserve before the possibly-blocking device-code request, both for the
	// capacity bound and so shutdown cannot miss in-flight construction.
	b.flowStarting[""]++
	b.flowStarting[req.Provider]++
	b.flowWG.Add(1)
	b.flowMu.Unlock()

	// The slot is given back by a defer, not by the lines below: beginFlow is a
	// network call into third-party code, and a panic there would otherwise
	// consume a slot for the life of the process and leave CloseBackend's Wait
	// blocked - meaning the daemon could never shut down.
	//
	// flowWG is a separate defer, and it is the outer one, so it is marked done
	// last of all. It is what CloseBackend waits on, and what it is waiting for
	// is not the slot but the teardown: a flow this request started holds the
	// OAuth callback port, and a Wait that returned before it was cancelled
	// would leave the next daemon unable to bind.
	defer b.flowWG.Done()
	release := func() {
		b.flowStarting[""]--
		b.flowStarting[req.Provider]--
	}
	locked := false
	defer func() {
		if !locked {
			b.flowMu.Lock()
			release()
			b.flowMu.Unlock()
		}
	}()

	// Started on a goroutine and waited for with a bound. The call reaches a
	// provider over the network, and the flow it produces has to outlive this
	// request - so it gets the daemon's context - but the *request* must not:
	// against a provider that never answers, the flow's own timeout is minutes,
	// and a browser cannot abort a handler that is not watching for it.
	begun := make(chan startedLogin, 1)
	go func() {
		f, err := b.beginFlow(b.flowCtx, req.Provider)
		begun <- startedLogin{provider: req.Provider, f: f, err: err}
	}()

	var got startedLogin
	select {
	case got = <-begun:
	case <-ctx.Done():
		// The reservation stays until the goroutine returns, and whatever it
		// produces is thrown away: a flow nobody can name is one nobody can
		// finish. The WaitGroup passes to the discard, so shutdown still waits
		// for the teardown this request walked away from.
		b.flowWG.Add(1)
		go b.discardLogin(begun)
		locked = true
		return web.Login{}, ctx.Err()
	case <-time.After(loginStartTimeout):
		b.flowWG.Add(1)
		go b.discardLogin(begun)
		locked = true
		return web.Login{}, web.Busy(req.Provider + " did not answer in time")
	}
	f, err := got.f, got.err
	b.flowMu.Lock()
	locked = true
	release()
	if err != nil {
		b.flowMu.Unlock()
		// A callback port already taken is another sign-in, not a fault: this
		// daemon does not own that port, and a terminal running `aigem auth
		// login` holds it too. Told as a refusal, with what to do about it.
		if errors.Is(err, auth.ErrLoginInProgress) {
			return web.Login{}, web.Refuse(errors.New(
				"another sign-in is already in progress; finish or cancel it first"))
		}
		return web.Login{}, err
	}
	if b.closed {
		b.flowMu.Unlock()
		// Torn down before this returns, and before the deferred Done: the flow
		// holds the callback port, and shutdown is what is waiting for it.
		f.Cancel()
		f.Wait()
		return web.Login{}, errors.New("the web backend is closed")
	}
	b.flowSeq++
	id := "LOGIN-" + strconv.FormatUint(b.flowSeq, 10)
	b.flows[id] = f
	b.flowOrder = append(b.flowOrder, id)
	b.flowWG.Add(1)
	b.flowMu.Unlock()
	go b.watchLogin(f)
	return loginView(id, f), nil
}

// discardLogin waits out a login start this daemon stopped waiting for, gives
// its slot back, and cancels whatever it produced. It is the other half of
// BeginLogin's bound: the reservation is held until the provider call returns,
// so abandoning one request cannot be used to start an unbounded number of
// outbound calls.
func (b *webBackend) discardLogin(begun <-chan startedLogin) {
	// Done last, after the flow is not only cancelled but finished: what
	// CloseBackend is waiting for is the callback listener being closed, not the
	// bookkeeping being tidy.
	defer b.flowWG.Done()
	got := <-begun
	b.flowMu.Lock()
	b.flowStarting[""]--
	b.flowStarting[got.provider]--
	b.flowMu.Unlock()
	if got.f != nil {
		got.f.Cancel()
		got.f.Wait()
	}
}

func (b *webBackend) Login(_ context.Context, id string) (web.Login, error) {
	f, err := b.loginFlow(id)
	if err != nil {
		return web.Login{}, err
	}
	return loginView(id, f), nil
}

func (b *webBackend) PasteLogin(_ context.Context, id, raw string) (web.Login, error) {
	f, err := b.loginFlow(id)
	if err != nil {
		return web.Login{}, err
	}
	if err := f.Paste(raw); err != nil {
		return web.Login{}, web.Refuse(err)
	}
	return loginView(id, f), nil
}

// CancelLogin abandons a login and keeps its record. The page that started it
// is still polling, and an id that vanished would answer 404 where the truth is
// "cancelled"; the record is evicted with the other finished ones instead.
func (b *webBackend) CancelLogin(_ context.Context, id string) error {
	f, err := b.loginFlow(id)
	if err != nil {
		return err
	}
	f.Cancel()
	return nil
}

func (b *webBackend) loginFlow(id string) (*auth.Flow, error) {
	b.flowMu.Lock()
	f := b.flows[id]
	b.flowMu.Unlock()
	if f == nil {
		return nil, web.ErrNoLogin
	}
	return f, nil
}

func (b *webBackend) watchLogin(f *auth.Flow) {
	defer b.flowWG.Done()
	f.Wait()
	state, err := f.Status()
	if state == auth.FlowDone {
		b.recordActivity(web.Activity{Kind: "auth.login", Text: "Signed in to " + f.Provider})
		b.publish("auth.updated", map[string]string{"provider": f.Provider})
	} else if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, auth.ErrFlowCancelled) {
		// Provider details stay in the daemon log and are emitted once here, at
		// the terminal transition, never from an idempotent status poll.
		slog.Warn("provider login failed", "provider", f.Provider, "err", err)
	}
	b.flowMu.Lock()
	b.cleanupFlowsLocked()
	b.flowMu.Unlock()
}

// loginView is what a browser is told about a login. The failure is a fixed
// string on purpose, and it is the one place in this file that differs from
// webRunError's argument for passing a reason through: a provider's own error
// carries its endpoints, its response body and, through the credential store,
// absolute paths. There is nothing in it a person can act on that "sign in
// again" does not already say, so the detail goes to the daemon's log once, at
// the terminal transition, and never from a status poll.
func loginView(id string, f *auth.Flow) web.Login {
	state, url, code, acceptsPaste, err := f.Snapshot()
	v := web.Login{
		ID: id, Provider: f.Provider, URL: url, Code: code,
		AcceptsPaste: acceptsPaste, State: string(state),
	}
	switch {
	case state == auth.FlowCancelled:
		v.Error = "cancelled"
	case err != nil:
		v.Error = "provider login failed"
	}
	return v
}

// cleanupFlowsLocked drops the oldest finished logins once there are more of
// them than anyone will look at. Only finished ones are dropped: a pending flow
// is a browser tab still waiting on an answer, and forgetting it would leave
// that tab polling an id this daemon no longer knows.
func (b *webBackend) cleanupFlowsLocked() {
	terminal := 0
	for _, id := range b.flowOrder {
		if f := b.flows[id]; f != nil {
			if state, _ := f.Status(); state != auth.FlowPending {
				terminal++
			}
		}
	}
	kept := b.flowOrder[:0]
	for _, id := range b.flowOrder {
		f := b.flows[id]
		if f != nil && terminal > maxRetainedTerminalLoginFlows {
			if state, _ := f.Status(); state != auth.FlowPending {
				delete(b.flows, id)
				terminal--
				continue
			}
		}
		kept = append(kept, id)
	}
	b.flowOrder = kept
}

func (b *webBackend) CloseBackend() {
	b.closeOnce.Do(func() {
		b.flowMu.Lock()
		b.closed = true
		b.flowCancel()
		flows := make([]*auth.Flow, 0, len(b.flows))
		for _, f := range b.flows {
			flows = append(flows, f)
		}
		b.flowMu.Unlock()
		for _, f := range flows {
			f.Cancel()
		}
		b.flowWG.Wait()
	})
}

const maxSkillPreview = 256 << 10

func (b *webBackend) Skills(context.Context) (web.Skills, error) {
	if b.env == nil {
		return web.Skills{Items: []web.SkillSummary{}}, nil
	}
	b.skillMu.Lock()
	defer b.skillMu.Unlock()
	out := web.Skills{Items: skillSummaries(b.env.Skills)}
	// Read from the project rather than from Env.Pending for the same reason
	// TrustSkills does: the snapshot is from startup, and a page that cannot see
	// a skill added since then has no way to ask for it. Memoised, because this
	// is a listing a page polls and the read behind it is the whole skill tree.
	// A discovery error is not worth failing the listing over - the catalog
	// above it is still true - so it is reported as nothing pending and logged.
	pending, err := b.pendingSkillsLocked()
	if err != nil {
		slog.Warn("the project's pending skills could not be read", "err", err)
	}
	if pending != nil {
		out.Pending = &web.PendingSkills{
			Names: append([]string(nil), pending.Names...), Invalidated: pending.Invalidated,
		}
	}
	return out, nil
}

func (b *webBackend) Skill(_ context.Context, name string) (web.Skill, error) {
	if b.env == nil {
		return web.Skill{}, web.ErrNoSkill
	}
	b.skillMu.Lock()
	defer b.skillMu.Unlock()
	sk, ok := b.env.Skills.Get(name)
	if !ok {
		return web.Skill{}, web.ErrNoSkill
	}
	paths := make([]string, 0, len(sk.Paths))
	for _, p := range sk.Paths {
		if !filepath.IsAbs(p) {
			paths = append(paths, p)
		}
	}
	body := sk.Body()
	truncated := len(body) > maxSkillPreview
	if truncated {
		body = body[:maxSkillPreview]
	}
	return web.Skill{
		SkillSummary: skillSummary(sk), WhenToUse: sk.WhenToUse,
		AllowedTools:    append([]string(nil), sk.AllowedTools...),
		DisallowedTools: append([]string(nil), sk.DisallowedTools...),
		Model:           sk.Model, Effort: sk.Effort, Context: sk.Context, Agent: sk.Agent,
		Paths: paths, Body: body, BodyTruncated: truncated,
	}, nil
}

func (b *webBackend) TrustSkills(context.Context) (web.SkillApproval, error) {
	if b.env == nil {
		return web.SkillApproval{}, web.Refuse(errors.New("no project environment is loaded"))
	}
	// The same lock protects every web read above and the full approval. The Env
	// synchronizes attached sessions internally; together they prevent a list or
	// detail response observing half of the catalog replacement.
	b.skillMu.Lock()
	defer b.skillMu.Unlock()
	// Refused before the work, not after it: approving again re-writes the trust
	// file, re-walks the project's skill directories under every session's lock
	// and appends to the feed, so a client holding down the button would stall
	// every open conversation for as long as it kept asking.
	//
	// Asked of the project and not of Env.Pending, which is the snapshot Load
	// took and is cleared by the first approval: a skill added or edited since
	// then is genuinely pending, and gating on the stale field would refuse the
	// only route that can pick it up, for the life of the daemon.
	//
	// Not memoised, unlike the listing: this is the mutation, and it must not
	// approve against an answer from a moment ago.
	pending, err := skill.Pending(b.env.Cwd)
	if err != nil {
		return web.SkillApproval{}, err
	}
	b.forgetPendingLocked()
	if pending == nil {
		return web.SkillApproval{}, web.Refuse(
			errors.New("this project has no skills awaiting approval"))
	}
	res, err := b.env.ApproveProjectSkills()
	if err != nil {
		// web.ErrBusy rather than a refusal: the request was fine and the answer
		// is to ask again, which is what 503 and Retry-After say and what 400
		// does not.
		if errors.Is(err, uisession.ErrBusy) {
			return web.SkillApproval{}, web.Busy("skills cannot change while a turn is running")
		}
		// Nothing to approve is a question with an answer, not a fault: the
		// project defines no skills, or the ones it defined are already trusted.
		if errors.Is(err, skill.ErrNoProjectSkills) {
			return web.SkillApproval{}, web.Refuse(
				errors.New("this project has no skills awaiting approval"))
		}
		// Discovery and persistence errors can carry absolute source paths. They
		// belong in the daemon log, not in this transport.
		return web.SkillApproval{}, err
	}
	out := web.SkillApproval{Loaded: append([]string(nil), res.Loaded...)}
	for _, notice := range res.Notices {
		// Discovery errors can include absolute definition paths. The browser only
		// needs to know that one definition was skipped; details stay in the log.
		slog.Warn("a skill definition was skipped after approval", "err", notice.Text)
		out.Notices = append(out.Notices, "A skill definition was skipped")
	}
	b.recordActivity(web.Activity{Kind: "skills.trusted", Text: "Approved project skills"})
	b.publish("skills.updated", out)
	return out, nil
}

// pendingSkillsLocked answers from the memo when it is fresh. Held under
// skillMu, which is also what serialises it against an approval.
func (b *webBackend) pendingSkillsLocked() (*skill.PendingSkills, error) {
	if !b.pendingAt.IsZero() && time.Since(b.pendingAt) < pendingSkillsTTL {
		return b.pendingVal, nil
	}
	got, err := skill.Pending(b.env.Cwd)
	if err != nil {
		return nil, err
	}
	b.pendingVal, b.pendingAt = got, time.Now()
	return got, nil
}

// forgetPendingLocked drops the memo, so the answer after an approval is the
// one the approval produced rather than the one it replaced.
func (b *webBackend) forgetPendingLocked() {
	b.pendingVal, b.pendingAt = nil, time.Time{}
}

func skillSummaries(reg *skill.Registry) []web.SkillSummary {
	if reg == nil {
		return []web.SkillSummary{}
	}
	list := reg.List()
	out := make([]web.SkillSummary, 0, len(list))
	for _, sk := range list {
		out = append(out, skillSummary(sk))
	}
	return out
}

func skillSummary(sk *skill.Skill) web.SkillSummary {
	return web.SkillSummary{
		Name: sk.Name, Description: sk.Description, ProjectLocal: sk.ProjectLocal, Builtin: sk.Builtin,
		UserInvocable: sk.UserInvocable, ModelInvocable: sk.ModelInvocable(),
		Conditional: sk.Conditional(), ArgumentHint: sk.ArgHint,
	}
}

func (b *webBackend) Commands(context.Context) ([]web.Command, error) {
	if b.env == nil {
		return []web.Command{}, nil
	}
	b.skillMu.Lock()
	cmds := uisession.Commands(b.env.Skills, b.env.MCP)
	b.skillMu.Unlock()
	out := make([]web.Command, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, web.Command{Name: c.Name, Description: c.Desc})
	}
	return out, nil
}

func (b *webBackend) Usage(context.Context) ([]web.ProviderUsage, error) {
	stored := llm.LoadLimits()
	providers := make(map[string]bool, len(stored))
	for name := range stored {
		providers[name] = true
	}
	for _, p := range b.models.Providers() {
		if p.NeedsAuth() && auth.IsAuthenticated(p.ID) {
			providers[p.ID] = true
		}
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]web.ProviderUsage, 0, len(names))
	for _, name := range names {
		l := stored[name]
		provider := l.Provider
		if provider == "" {
			provider = name
		}
		v := web.ProviderUsage{
			Provider: provider, Model: l.Model, Plan: l.Plan, Credits: l.Credits,
			ObservedAt: l.ObservedAt, Windows: make([]web.LimitWindow, 0, len(l.Windows)),
		}
		for _, w := range l.Windows {
			v.Windows = append(v.Windows, web.LimitWindow{
				Name: w.Name, UsedPercent: w.UsedPercent, WindowMinutes: w.WindowMinutes,
				ResetAt: w.ResetAt, Remaining: w.Remaining,
			})
		}
		out = append(out, v)
	}
	return out, nil
}

func (b *webBackend) Activity(_ context.Context, since uint64, limit int) ([]web.Activity, error) {
	if b.activity == nil {
		return []web.Activity{}, nil
	}
	// The wire cursor is a uint64 and the log's is an int. A cursor past what an
	// int holds names no entry that can exist, so the page after it is empty.
	if since > math.MaxInt {
		return []web.Activity{}, nil
	}
	entries, err := b.activity.Range(int(since), limit)
	if err != nil {
		return nil, err
	}
	out := make([]web.Activity, 0, len(entries))
	for _, e := range entries {
		v := e.V
		v.Seq, v.At = uint64(e.Seq), e.At
		out = append(out, v)
	}
	return out, nil
}

// recordActivity is the single owner of activity ordering: the durable append
// succeeds before a control update can claim the collection changed.
func (b *webBackend) recordActivity(v web.Activity) bool {
	b.activityMu.Lock()
	defer b.activityMu.Unlock()
	if b.activity == nil {
		return false
	}
	v.Seq, v.At = 0, v.At.UTC()
	if _, err := b.activity.Append(v); err != nil {
		slog.Error("the activity feed could not be appended", "kind", v.Kind, "err", err)
		return false
	}
	b.publish("activity.updated", map[string]string{"kind": v.Kind})
	return true
}

func (b *webBackend) publish(kind string, data any) {
	if b.notify != nil {
		b.notify(kind, data)
	}
}
