package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	maxActivityPage   = 1000
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
	Skills(context.Context) (Skills, error)
	Skill(context.Context, string) (Skill, error)
	TrustSkills(context.Context) (SkillApproval, error)
}

type CommandsBackend interface {
	Commands(context.Context) ([]Command, error)
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
	b, ok := s.backend.(ModelsBackend)
	if !ok {
		unavailable(w)
		return
	}
	items, err := b.Models(r.Context())
	if err != nil {
		writePhaseError(w, "listing models", err)
		return
	}
	if items == nil {
		items = []Model{}
	}
	writeJSON(w, items)
}

func (s *Server) handleDefaultModel(w http.ResponseWriter, r *http.Request) {
	b, ok := s.backend.(ModelsBackend)
	if !ok {
		unavailable(w)
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
	b, ok := s.backend.(AuthBackend)
	if !ok {
		unavailable(w)
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
	b, ok := s.backend.(AuthBackend)
	if !ok {
		unavailable(w)
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
	b, ok := s.backend.(AuthBackend)
	if !ok {
		unavailable(w)
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
	b, ok := s.backend.(AuthBackend)
	if !ok {
		unavailable(w)
		return
	}
	if err := b.CancelLogin(r.Context(), r.PathValue("id")); err != nil {
		writePhaseError(w, "cancelling provider login", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	b, ok := s.backend.(SkillsBackend)
	if !ok {
		unavailable(w)
		return
	}
	v, err := b.Skills(r.Context())
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
	b, ok := s.backend.(SkillsBackend)
	if !ok {
		unavailable(w)
		return
	}
	v, err := b.Skill(r.Context(), r.PathValue("name"))
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
	b, ok := s.backend.(SkillsBackend)
	if !ok {
		unavailable(w)
		return
	}
	// This mutation has no options. An empty body and {} are accepted; any field,
	// second document or oversized body is still a malformed request.
	var req struct{}
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	v, err := b.TrustSkills(r.Context())
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
	b, ok := s.backend.(CommandsBackend)
	if !ok {
		unavailable(w)
		return
	}
	v, err := b.Commands(r.Context())
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
	b, ok := s.backend.(UsageBackend)
	if !ok {
		unavailable(w)
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
	since, ok := cursor(w, r, "since")
	if !ok {
		return
	}
	limit, ok := count(w, r, "limit", maxActivityPage)
	if !ok {
		return
	}
	b, ok := s.backend.(ActivityBackend)
	if !ok {
		unavailable(w)
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

func unavailable(w http.ResponseWriter) {
	http.Error(w, "this endpoint is not available", http.StatusNotImplemented)
}

func writePhaseError(w http.ResponseWriter, doing string, err error) {
	var refusal *Refusal
	switch {
	case errors.Is(err, ErrNoLogin), errors.Is(err, ErrNoSkill):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBusy):
		w.Header().Set("Retry-After", "5")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.As(err, &refusal):
		http.Error(w, refusal.Reason, http.StatusBadRequest)
	default:
		slog.Error("the daemon failed a request", "doing", doing, "err", err)
		http.Error(w, "the daemon could not carry that out", http.StatusInternalServerError)
	}
}
