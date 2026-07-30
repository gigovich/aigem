package main

import "testing"

func TestRunModelsCommandUnknownSubcommand(t *testing.T) {
	if err := runModelsCommand([]string{"bogus"}); err == nil {
		t.Error("expected error for unknown subcommand")
	}
}

func TestRunModelsCommandStatusNoConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// status must not error when the local model is uninitialized.
	if err := runModelsCommand([]string{"status"}); err != nil {
		t.Errorf("status with no config: %v", err)
	}
}
