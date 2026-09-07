package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/llm"
)

// FlowState is the externally useful state of a browser-driven login.
type FlowState string

const (
	FlowPending FlowState = "pending"
	FlowDone    FlowState = "done"
	FlowFailed  FlowState = "failed"
	// FlowCancelled is its own state and not a failure: a page polling a login
	// the person abandoned must not be shown "provider login failed" for
	// something the person did on purpose.
	FlowCancelled FlowState = "cancelled"
)

// ErrFlowCancelled is the terminal result of explicitly abandoning a flow.
var ErrFlowCancelled = errors.New("cancelled")

// ErrLoginInProgress is returned when a login cannot start because the one
// callback port its provider redirects to is already taken. It is a sentinel
// because it is the one way a login start fails that is not a fault: another
// sign-in is under way, here or in a terminal, and the answer is to finish or
// cancel that one rather than anything about this daemon.
var ErrLoginInProgress = errors.New("another sign-in is already in progress")

// Flow is a provider login which continues independently of the request that
// started it. Its public fields contain display data only, never credentials or
// token endpoints.
type Flow struct {
	Provider     string
	URL          string
	Code         string
	AcceptsPaste bool

	mu       sync.Mutex
	state    FlowState
	err      error
	cb       *callbackServer
	cancel   context.CancelFunc
	done     chan struct{}
	doneOnce sync.Once
}

// Status reports the current state. The error is meaningful for FlowFailed and
// for FlowCancelled, which carries ErrFlowCancelled.
func (f *Flow) Status() (FlowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.err
}

// Snapshot reads state and the non-secret display values at one instant, which
// is what an API response needs: a caller assembling them from separate reads
// could show a pending flow's authorization URL beside its failure. The URL and
// the code are cleared at a terminal transition, so neither lingers in a
// retained record.
func (f *Flow) Snapshot() (state FlowState, url, code string, acceptsPaste bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.URL, f.Code, f.AcceptsPaste, f.err
}

// Wait blocks until the OAuth goroutine has returned and any callback listener
// has been closed. Status may become terminal slightly before Wait returns.
func (f *Flow) Wait() {
	if f.done != nil {
		<-f.done
	}
}

func (f *Flow) complete() { f.doneOnce.Do(func() { close(f.done) }) }

func (f *Flow) finish(rec Record, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != FlowPending {
		return
	}
	if err == nil {
		if err = Put(f.Provider, rec); err == nil {
			ResetSources()
		}
	}
	f.URL, f.Code = "", ""
	if err != nil {
		f.state, f.err = FlowFailed, err
		return
	}
	f.state = FlowDone
}

// Paste delivers a redirect URL or bare code brought back from another device.
func (f *Flow) Paste(raw string) error {
	f.mu.Lock()
	cb, state := f.cb, f.state
	f.mu.Unlock()
	if cb == nil || !f.AcceptsPaste {
		return errors.New("this login does not take a pasted URL")
	}
	if state != FlowPending {
		return errors.New("this login is already finished")
	}
	if !cb.paste(raw) {
		return errors.New("could not read an authorization code out of that")
	}
	return nil
}

// Cancel abandons the login without changing the credential store.
func (f *Flow) Cancel() {
	f.mu.Lock()
	cancel := f.cancel
	if f.state == FlowPending {
		f.state, f.err = FlowCancelled, ErrFlowCancelled
		f.URL, f.Code = "", ""
	}
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// NewPendingFlow is a login with no provider behind it: it stays pending until
// its context is cancelled or Cancel is called, and it never writes a
// credential.
//
// It exists because everything a daemon does *around* a login - the caps on how
// many may be in flight, evicting the finished ones, cancelling them all at
// shutdown - is bookkeeping that has nothing to do with any provider, and the
// only other way to reach it is to bind the fixed callback port and talk to
// one.
func NewPendingFlow(ctx context.Context, provider string) *Flow {
	ctx, cancel := context.WithCancel(ctx)
	f := &Flow{Provider: provider, state: FlowPending, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer f.complete()
		defer cancel()
		<-ctx.Done()
		f.finish(Record{}, ErrFlowCancelled)
	}()
	return f
}

// Begin starts an interactive login. Its caller must give it an ownership
// context rather than a request context; Cancel or cancellation of that context
// owns the asynchronous lifetime.
func Begin(ctx context.Context, provider string) (*Flow, error) {
	switch provider {
	case llm.XAIProviderID:
		return beginXAIDevice(ctx)
	case llm.OpenAIProviderID:
		return beginChatGPT(ctx)
	default:
		return nil, fmt.Errorf("%s has no interactive login; add an API key with `aigem auth login %s`", provider, provider)
	}
}

func beginXAIDevice(ctx context.Context) (*Flow, error) {
	ctx, cancel := context.WithTimeout(ctx, xaiLoginTimeout)
	tokenURL, deviceURL := xaiDiscovery(ctx)
	cfg := &oauth2.Config{
		ClientID: xaiClientID,
		Endpoint: oauth2.Endpoint{DeviceAuthURL: deviceURL, TokenURL: tokenURL},
		Scopes:   xaiScopes,
	}
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("xai device-code request: %w", err)
	}
	verifyURL := da.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = da.VerificationURI
	}
	f := &Flow{
		Provider: llm.XAIProviderID, URL: verifyURL, Code: da.UserCode,
		state: FlowPending, cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer f.complete()
		defer cancel()
		tok, err := cfg.DeviceAccessToken(ctx, da)
		if err != nil {
			f.finish(Record{}, fmt.Errorf("waiting for xai authorization: %w", err))
			return
		}
		f.finish(Record{Kind: KindOAuth, Token: tok, TokenURL: tokenURL}, nil)
	}()
	return f, nil
}

func beginChatGPT(ctx context.Context) (*Flow, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	state, err := randState()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("generate state: %w", err)
	}
	cb, err := startCallback(chatGPTRedirect, state, false)
	if err != nil {
		cancel()
		return nil, err
	}
	cfg := oauthConfig()
	verifier := oauth2.GenerateVerifier()
	authURL := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("id_token_add_organizations", "true"))
	f := &Flow{
		Provider: llm.OpenAIProviderID, URL: authURL, AcceptsPaste: true,
		state: FlowPending, cb: cb, cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer f.complete()
		defer cancel()
		defer cb.close()
		res, err := cb.wait(ctx)
		if err != nil {
			f.finish(Record{}, fmt.Errorf("waiting for authorization: %w", err))
			return
		}
		if res.err != "" {
			f.finish(Record{}, fmt.Errorf("authorization denied: %s", res.err))
			return
		}
		if !stateOK(res, state) {
			f.finish(Record{}, errors.New("authorization state mismatch (possible CSRF)"))
			return
		}
		clientCtx := context.WithValue(ctx, oauth2.HTTPClient, http.DefaultClient)
		tok, err := cfg.Exchange(clientCtx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			f.finish(Record{}, fmt.Errorf("token exchange: %w", err))
			return
		}
		rec := Record{Kind: KindOAuth, Token: tok}
		if idTok, ok := tok.Extra("id_token").(string); ok {
			rec.AccountID = accountIDFromIDToken(idTok)
		}
		f.finish(rec, nil)
	}()
	return f, nil
}
