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

// A login driven by something other than a terminal.
//
// The two flows behave differently once the browser is not on the machine
// running aigem, and the difference is not cosmetic. The xAI flow is
// device-code: it shows a URL and a code, and the daemon polls, so it works
// from a phone unchanged. The ChatGPT flow is authorization-code against a
// pre-registered loopback redirect, so the provider sends the browser to
// *that browser's* localhost - which is this machine only when the two are the
// same machine. From anywhere else the user has to bring the redirected URL
// back by hand, which is what Paste is for.

// FlowState is where a login has got to.
type FlowState string

const (
	FlowPending FlowState = "pending"
	FlowDone    FlowState = "done"
	FlowFailed  FlowState = "failed"
)

// Flow is a login in progress. The fields describing what to show the user are
// set before it is returned and never change; the outcome is read with Status.
type Flow struct {
	Provider string
	// URL is what the user opens. Code is the device code to confirm, when the
	// flow has one.
	URL  string
	Code string
	// AcceptsPaste means the redirect cannot be relied on to come back here, so
	// the user may have to return it by hand.
	AcceptsPaste bool

	mu     sync.Mutex
	state  FlowState
	err    error
	cb     *callbackServer
	cancel context.CancelFunc
}

// Status reports progress. The error is only meaningful once the state is
// FlowFailed.
func (f *Flow) Status() (FlowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.err
}

func (f *Flow) finish(rec Record, err error) {
	// Claim the outcome before writing anything. The exchange can return at the
	// same moment the user cancels, and Cancel promises the credential store is
	// untouched - persisting after losing that race would break the promise and
	// report "cancelled" over a login that had in fact replaced the credential.
	f.mu.Lock()
	if f.state != FlowPending {
		f.mu.Unlock()
		return
	}
	if err == nil {
		f.state = FlowDone
	} else {
		f.state, f.err = FlowFailed, err
	}
	f.mu.Unlock()

	if err == nil {
		if err = Put(f.Provider, rec); err == nil {
			// A cached token source for this provider now holds the credential
			// that was just replaced.
			ResetSources()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.state, f.err = FlowFailed, err
	}
}

// Paste delivers a redirect URL (or a bare code) the user brought back by hand.
// It is refused for a flow that has no callback waiting for one, so a device
// flow cannot be fed a code from somewhere else.
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

// Cancel abandons a login. The credential store is untouched: a flow that never
// completed never wrote to it.
func (f *Flow) Cancel() {
	f.mu.Lock()
	cancel := f.cancel
	if f.state == FlowPending {
		f.state, f.err = FlowFailed, errors.New("cancelled")
	}
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// BeginXAIDevice starts the Grok subscription login. The device-code exchange
// happens up front so the URL and code are known before this returns; the wait
// for approval runs on its own.
func BeginXAIDevice(ctx context.Context) (*Flow, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), xaiLoginTimeout)

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
		Provider: llm.XAIProviderID,
		URL:      verifyURL,
		Code:     da.UserCode,
		state:    FlowPending,
		cancel:   cancel,
	}
	go func() {
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

// BeginChatGPT starts the ChatGPT login. The loopback callback is bound before
// this returns, so a browser on this machine completes it without further help;
// a browser anywhere else brings the redirected URL back through Paste.
func BeginChatGPT(ctx context.Context) (*Flow, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)

	cfg := oauthConfig()
	verifier := oauth2.GenerateVerifier()
	state, err := randState()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("generate state: %w", err)
	}
	// No stdin reader: this flow has no terminal, and reading stdin from a
	// daemon would steal input from whatever else is using it.
	cb, err := startCallback(chatGPTRedirect, state, false)
	if err != nil {
		cancel()
		return nil, err
	}
	authURL := cfg.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("id_token_add_organizations", "true"))

	f := &Flow{
		Provider:     llm.OpenAIProviderID,
		URL:          authURL,
		AcceptsPaste: true,
		state:        FlowPending,
		cb:           cb,
		cancel:       cancel,
	}
	go func() {
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
		// The same CSRF rule as the terminal flow: a callback that arrived over
		// HTTP must echo the state exactly, and a pasted URL bearing one must
		// match it too.
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

// Begin starts the login for a provider that has an interactive flow.
func Begin(ctx context.Context, provider string) (*Flow, error) {
	switch provider {
	case llm.XAIProviderID:
		return BeginXAIDevice(ctx)
	case llm.OpenAIProviderID:
		return BeginChatGPT(ctx)
	default:
		return nil, fmt.Errorf("%s has no interactive login; add an API key with `aigem auth login %s`",
			provider, provider)
	}
}
