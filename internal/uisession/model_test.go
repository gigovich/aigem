package uisession

import (
	"strings"
	"testing"
)

// A session built without a model registry cannot resolve a reference. It has
// to say so: dereferencing the nil registry would panic on the caller's
// goroutine, and in a process holding several conversations that takes all of
// them down over one client's request.
func TestSwitchModelWithoutARegistryIsAnError(t *testing.T) {
	l := newSession(t)

	_, err := l.SwitchModel("openai/gpt-5.6-sol", false)
	if err == nil {
		t.Fatal("switching model without a registry was accepted")
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}
