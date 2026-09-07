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
)

// ErrFlowCancelled is the terminal result of explicitly abandoning a flow.
var ErrFlowCancelled = errors.New("cancelled")

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

// Status reports the current state. The error is meaningful only for FlowFailed.
func (f *Flow) Status() (FlowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.err
}

// Display reports the non-secret values a front-end may show. They are cleared
// at a terminal transition so an authorization URL cannot linger in retained
// flow records.
func (f *Flow) Display() (url, code string, acceptsPaste bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.URL, f.Code, f.AcceptsPaste
}

// Snapshot reads status and display data at one instant for an API response.
func (f *Flow) Snapshot() (FlowState, string, string, bool, error) {
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
		f.state, f.err = FlowFailed, ErrFlowCancelled
		f.URL, f.Code = "", ""
	}
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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
