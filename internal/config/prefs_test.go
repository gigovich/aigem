package config

import "testing"

func TestPrefsRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if got := LoadPrefs(); got.Model != "" {
		t.Fatalf("no prefs file should yield zero Prefs, got %+v", got)
	}
	if err := SaveModelPref("openai/gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	if got := LoadPrefs(); got.Model != "openai/gpt-5.6-sol" {
		t.Fatalf("expected saved model, got %q", got.Model)
	}
	// A second save overwrites the model but the file stays valid.
	if err := SaveModelPref("local/gemma"); err != nil {
		t.Fatal(err)
	}
	if got := LoadPrefs(); got.Model != "local/gemma" {
		t.Fatalf("expected overwritten model, got %q", got.Model)
	}
}
