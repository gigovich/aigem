package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// The rest of the phase-one API: what a page needs to choose a model, sign in
// to a provider, see the project's skills and read what has happened.
//
// Everything here is small, whole documents rather than streams: none of it
// changes fast enough to be worth a socket, and a page that has just been told
// through the control stream that something moved refetches the one collection
// it names. The seams are declared per endpoint group, because a fake built for
// one screen must not have to implement the other six.

const (
	// A page shows a feed, not a database. The cap is what one screenful's worth
	// of scrollback costs to encode; a client that wants more asks again from
	// the cursor it was given.
	maxActivityPage = 1000
	// A pasted provider callback is a URL or a code. Eight kibibytes is far more
	// than either, and small enough that a client cannot make the daemon hold a
	// body worth holding.
	maxLoginPasteBody = 8 << 10
)

var (
	ErrNoLogin = errors.New("web: no such login")
	ErrNoSkill = errors.New("web: no such skill")
)

// The interfaces below are intentionally per endpoint group. Adding a screen
// must not make every run-only fake implement unrelated model, auth and skill
// methods.
type ModelsBackend interface {
	Models(context.Context) ([]Model, error)
	SetDefaultModel(context.Context, string) (Model, error)
}

type AuthBackend interface {
	BeginLogin(context.Context, LoginRequest) (Login, error)
	Login(context.Context, string) (Login, error)
	PasteLogin(context.Context, string, string) (Login, error)
	CancelLogin(context.Context, string) error
}

type SkillsBackend interface {
	Skills(ctx context.Context, project string) (Skills, error)
	Skill(ctx context.Context, project, name string) (Skill, error)
	TrustSkills(ctx context.Context, project string) (SkillApproval, error)
}

type CommandsBackend interface {
	Commands(ctx context.Context, project string) ([]Command, error)
}
type UsageBackend interface {
	Usage(context.Context) ([]ProviderUsage, error)
}
type ActivityBackend interface {
	Activity(context.Context, uint64, int) ([]Activity, error)
}

// BackendShutdown is implemented by an adapter which owns asynchronous work,
// such as provider logins. Server.Close invokes it once.
type BackendShutdown interface{ CloseBackend() }

type Model struct {
	Ref           string `json:"ref"`
	Provider      string `json:"provider"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
	NeedsAuth     bool   `json:"needsAuth"`
	Authenticated bool   `json:"authenticated"`
	Default       bool   `json:"default"`
}

type LoginRequest struct {
	Provider string `json:"provider"`
}

type Login struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	URL          string `json:"url"`
	Code         string `json:"code,omitempty"`
	AcceptsPaste bool   `json:"acceptsPaste,omitempty"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
}

type SkillSummary struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	ProjectLocal   bool   `json:"projectLocal,omitempty"`
	Builtin        bool   `json:"builtin,omitempty"`
	UserInvocable  bool   `json:"userInvocable"`
	ModelInvocable bool   `json:"modelInvocable"`
	Conditional    bool   `json:"conditional,omitempty"`
	ArgumentHint   string `json:"argumentHint,omitempty"`
}

type Skill struct {
	SkillSummary
	WhenToUse       string   `json:"whenToUse,omitempty"`
	AllowedTools    []string `json:"allowedTools"`
	DisallowedTools []string `json:"disallowedTools"`
	Model           string   `json:"model,omitempty"`
	Effort          string   `json:"effort,omitempty"`
	Context         string   `json:"context,omitempty"`
	Agent           string   `json:"agent,omitempty"`
	Paths           []string `json:"paths"`
	Body            string   `json:"body"`
	BodyTruncated   bool     `json:"bodyTruncated,omitempty"`
}

type PendingSkills struct {
	Names       []string `json:"names"`
	Invalidated bool     `json:"invalidated,omitempty"`
}

type Skills struct {
	Items   []SkillSummary `json:"items"`
	Pending *PendingSkills `json:"pending,omitempty"`
}

type SkillApproval struct {
	Loaded  []string `json:"loaded"`
	Notices []string `json:"notices"`
}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type LimitWindow struct {
	Name          string    `json:"name"`
	UsedPercent   float64   `json:"usedPercent,omitempty"`
	WindowMinutes int       `json:"windowMinutes,omitempty"`
	ResetAt       time.Time `json:"resetAt,omitzero"`
	Remaining     string    `json:"remaining,omitempty"`
}

type ProviderUsage struct {
	Provider   string        `json:"provider"`
	Model      string        `json:"model,omitempty"`
	Plan       string        `json:"plan,omitempty"`
	Credits    string        `json:"credits,omitempty"`
	Windows    []LimitWindow `json:"windows"`
	ObservedAt time.Time     `json:"observedAt,omitzero"`
}

type Activity struct {
	Seq    uint64    `json:"seq,omitempty"`
	At     time.Time `json:"at,omitzero"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	RunRef string    `json:"runRef,omitempty"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ModelsBackend](s, w, "models")
	if !ok {
		return
	}
	items, err := b.Models(r.Context())
	if err != nil {
		writePhaseError(w, "listing models", err)
		return
	}
	// Empty is an empty collection and never null, here and in every handler
	// below: the reason is the one given over writeJSON's own use of it in
	// api_runs.go - a page that iterates what it was given should not have to
	// test each field for null first.
	if items == nil {
		items = []Model{}
	}
	writeJSON(w, items)
}

func (s *Server) handleDefaultModel(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[ModelsBackend](s, w, "models")
	if !ok {
		return
	}
	var req struct {
		Ref string `json:"ref"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Ref == "" {
		http.Error(w, "ref is required", http.StatusBadRequest)
		return
	}
	m, err := b.SetDefaultModel(r.Context(), req.Ref)
	if err != nil {
		writePhaseError(w, "setting the default model", err)
		return
	}
	writeJSON(w, m)
}

func (s *Server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[AuthBackend](s, w, "providerLogin")
	if !ok {
		return
	}
	var req LoginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}
	v, err := b.BeginLogin(r.Context(), req)
	if err != nil {
		writePhaseError(w, "starting provider login", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, v)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[AuthBackend](s, w, "providerLogin")
	if !ok {
		return
	}
	v, err := b.Login(r.Context(), r.PathValue("id"))
	if err != nil {
		writePhaseError(w, "reading provider login", err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) handleLoginPaste(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[AuthBackend](s, w, "providerLogin")
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginPasteBody))
	if err != nil {
		http.Error(w, "bad request: pasted value is too large", http.StatusBadRequest)
		return
	}
	v, err := b.PasteLogin(r.Context(), r.PathValue("id"), string(body))
	if err != nil {
		writePhaseError(w, "pasting a provider login callback", err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[AuthBackend](s, w, "providerLogin")
	if !ok {
		return
	}
	if err := b.CancelLogin(r.Context(), r.PathValue("id")); err != nil {
		writePhaseError(w, "cancelling provider login", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[SkillsBackend](s, w, "skills")
	if !ok {
		return
	}
	v, err := b.Skills(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		writePhaseError(w, "listing skills", err)
		return
	}
	if v.Items == nil {
		v.Items = []SkillSummary{}
	}
	if v.Pending != nil && v.Pending.Names == nil {
		v.Pending.Names = []string{}
	}
	writeJSON(w, v)
}

// handleSkillPath is one guarded route for two shapes, because ServeMux treats
// a literal segment and a wildcard at the same position as a conflict.
//
// The cost is that a skill actually named "trust" cannot be read through this
// route: GET on it is a 405 naming the mutation. Nothing shadows the mutation
// in the other direction - it needs POST - and the name is documented, so a
// project that hits this is told why rather than left with a skill that is
// listed and cannot be opened.
func (s *Server) handleSkillPath(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("name") == "trust" {
		if r.Method != http.MethodPost {
			methodNotAllowed("POST")(w, r)
			return
		}
		s.handleSkillTrust(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed("GET, HEAD")(w, r)
		return
	}
	s.handleSkill(w, r)
}

func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[SkillsBackend](s, w, "skills")
	if !ok {
		return
	}
	v, err := b.Skill(r.Context(), r.URL.Query().Get("project"), r.PathValue("name"))
	if err != nil {
		writePhaseError(w, "reading a skill", err)
		return
	}
	if v.AllowedTools == nil {
		v.AllowedTools = []string{}
	}
	if v.DisallowedTools == nil {
		v.DisallowedTools = []string{}
	}
	if v.Paths == nil {
		v.Paths = []string{}
	}
	writeJSON(w, v)
}

func (s *Server) handleSkillTrust(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[SkillsBackend](s, w, "skills")
	if !ok {
		return
	}
	// The one option is which project. An empty body and {} mean the daemon's
	// own directory; any other field, a second document or an oversized body is
	// still a malformed request.
	var req struct {
		Project string `json:"project,omitempty"`
	}
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	v, err := b.TrustSkills(r.Context(), req.Project)
	if err != nil {
		writePhaseError(w, "approving project skills", err)
		return
	}
	if v.Loaded == nil {
		v.Loaded = []string{}
	}
	if v.Notices == nil {
		v.Notices = []string{}
	}
	writeJSON(w, v)
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[CommandsBackend](s, w, "commands")
	if !ok {
		return
	}
	v, err := b.Commands(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		writePhaseError(w, "listing commands", err)
		return
	}
	if v == nil {
		v = []Command{}
	}
	writeJSON(w, v)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[UsageBackend](s, w, "usage")
	if !ok {
		return
	}
	v, err := b.Usage(r.Context())
	if err != nil {
		writePhaseError(w, "reading usage", err)
		return
	}
	if v == nil {
		v = []ProviderUsage{}
	}
	for i := range v {
		if v[i].Windows == nil {
			v[i].Windows = []LimitWindow{}
		}
	}
	writeJSON(w, v)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	// The seam first, as everywhere else: a client probing what this daemon can
	// do must not be told its query string was wrong by a route that does not
	// exist here at all.
	b, ok := backendOf[ActivityBackend](s, w, "activity")
	if !ok {
		return
	}
	since, ok := cursor(w, r, "since")
	if !ok {
		return
	}
	limit, ok := count(w, r, "limit", maxActivityPage)
	if !ok {
		return
	}
	v, err := b.Activity(r.Context(), since, limit)
	if err != nil {
		writePhaseError(w, "reading activity", err)
		return
	}
	if v == nil {
		v = []Activity{}
	}
	writeJSON(w, v)
}

// backendOf selects the seam a route needs, and is the single gate on whether
// this daemon serves that route at all.
//
// Two things can make it not: the backend may not implement the seam, or it may
// implement it and have been handed nothing to work with - no state directory,
// no loaded project. Both are the same fact to a page, both are 501, and both
// are already decided in s.features, which /api/meta answers from. Asking the
// feature map here rather than keeping a second list of the same fact is what
// stops the route and the map from drifting apart.
//
// It is asked before the query string is read, so a bad cursor on a route this
// daemon does not serve is 501 and not a complaint about the cursor.
func backendOf[T any](s *Server, w http.ResponseWriter, feature string) (T, bool) {
	b, ok := s.backend.(T)
	if !ok || !s.features[feature] {
		var zero T
		unavailable(w)
		return zero, false
	}
	return b, true
}

func unavailable(w http.ResponseWriter) {
	http.Error(w, "this endpoint is not available", http.StatusNotImplemented)
}

// writePhaseError is writeRunError with the two sentinels this file owns in
// front of it. Everything else - the busy answer, a refusal written to be read,
// and the daemon's own faults being described to nobody - is the same decision
// for the same reasons, and having it here twice is how the two halves of one
// API end up answering the same failure differently.
func writePhaseError(w http.ResponseWriter, doing string, err error) {
	switch {
	case errors.Is(err, ErrNoLogin):
		http.Error(w, "no such login", http.StatusNotFound)
	case errors.Is(err, ErrNoSkill):
		http.Error(w, "no such skill", http.StatusNotFound)
	default:
		writeRunError(w, doing, err)
	}
}
