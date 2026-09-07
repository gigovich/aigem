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
	// Whether a provider has a credential at all is asked once per provider:
	// reading it is a locked parse of the whole credential file, and asking per
	// model would make listing thirty models thirty of them, contending with the
	// write a completing login performs.
	//
	// Whether a *model* can be opened still has to be asked per model, and only
	// for a provider that has a credential: an OAuth login covers some of its
	// models and not others, and a model outside that set is not one this daemon
	// can send a turn to however signed in the provider is.
	credentialed := make(map[string]bool, len(infos))
	out := make([]web.Model, 0, len(infos))
	for _, info := range infos {
		p, _, err := b.models.Resolve(info.Ref())
		if err != nil {
			continue
		}
		has, known := credentialed[p.ID]
		if !known {
			has = !p.NeedsAuth() || auth.IsAuthenticated(p.ID)
			credentialed[p.ID] = has
		}
		ok := has
		if has && p.NeedsAuth() {
			_, _, _, openErr := auth.OpenModel(b.models, info.Ref(), defaultMaxTokens)
			ok = openErr == nil
		}
		out = append(out, web.Model{
			Ref: info.Ref(), Provider: p.ID, Name: info.Name,
			ContextWindow: info.ContextWindow, MaxTokens: info.MaxTokens, Reasoning: info.Reasoning,
			NeedsAuth: p.NeedsAuth(), Authenticated: ok,
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
	maxPendingLogins              = 8
	maxPendingLoginsPerProvider   = 4
	maxRetainedTerminalLoginFlows = 32
)

func (b *webBackend) BeginLogin(_ context.Context, req web.LoginRequest) (web.Login, error) {
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
	if global >= maxPendingLogins || provider >= maxPendingLoginsPerProvider {
		b.flowMu.Unlock()
		return web.Login{}, web.ErrBusy
	}
	// Reserve before the possibly-blocking device-code request, both for the
	// capacity bound and so shutdown cannot miss in-flight construction.
	b.flowStarting[""]++
	b.flowStarting[req.Provider]++
	b.flowWG.Add(1)
	b.flowMu.Unlock()

	// Released by a defer, not by the lines below: beginFlow is a network call
	// into third-party code, and a panic there would otherwise consume a slot
	// for the life of the process and leave CloseBackend's Wait blocked -
	// meaning the daemon could never shut down.
	release := func() {
		b.flowStarting[""]--
		b.flowStarting[req.Provider]--
		b.flowWG.Done()
	}
	locked := false
	defer func() {
		if !locked {
			b.flowMu.Lock()
			release()
			b.flowMu.Unlock()
		}
	}()

	f, err := b.beginFlow(b.flowCtx, req.Provider)
	b.flowMu.Lock()
	locked = true
	release()
	if err != nil {
		b.flowMu.Unlock()
		return web.Login{}, err
	}
	if b.closed {
		b.flowMu.Unlock()
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
	if err != nil {
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
	if b.env.Pending != nil {
		out.Pending = &web.PendingSkills{
			Names: append([]string(nil), b.env.Pending.Names...), Invalidated: b.env.Pending.Invalidated,
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
	if b.env.Pending == nil {
		return web.SkillApproval{}, web.Refuse(
			errors.New("this project has no skills awaiting approval"))
	}
	res, err := b.env.ApproveProjectSkills()
	if err != nil {
		// web.ErrBusy rather than a refusal: the request was fine and the answer
		// is to ask again, which is what 503 and Retry-After say and what 400
		// does not.
		if errors.Is(err, uisession.ErrBusy) {
			return web.SkillApproval{}, fmt.Errorf(
				"skills cannot change while a turn is running: %w", web.ErrBusy)
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
