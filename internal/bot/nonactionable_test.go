package bot

import (
	"errors"
	"testing"
)

func TestIsNonActionableErr(t *testing.T) {
	nonActionable := []error{
		errors.New(`responses: stream error: {"code":"context_length_exceeded"}`),
		errors.New("Your input exceeds the context window of this model"),
		errors.New(`{"type":"invalid_request_error"}`),
	}
	for _, e := range nonActionable {
		if !isNonActionableErr(e) {
			t.Errorf("expected non-actionable: %v", e)
		}
	}
	actionable := []error{
		errors.New("tool bash failed: exit status 1"),
		errors.New("no token stored for bot"),
		// An OAuth failure carries an "invalid_request_error" body but is an auth
		// problem, not a context-window overflow - it must not read as "too big".
		errors.New(`responses: auth: oauth2: cannot fetch token: 401 Unauthorized: ` +
			`{"error":{"type":"invalid_request_error","code":"refresh_token_reused"}}`),
	}
	for _, e := range actionable {
		if isNonActionableErr(e) {
			t.Errorf("expected actionable: %v", e)
		}
	}
}
