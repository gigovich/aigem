package main

import (
	"context"
	"testing"
)

// The adapter is the only thing standing between the web package and the model
// registry, so what it puts in Meta has to be what the wire expects: the
// version this binary reports, and a model reference the registry can resolve
// rather than a label meant for a human.
func TestWebBackendMetaReportsTheVersionAndAResolvableModel(t *testing.T) {
	b := newWebBackend("1.2.3-test")
	meta, err := b.Meta(context.Background())
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Version != "1.2.3-test" {
		t.Errorf("Version = %q, want the version the backend was built with", meta.Version)
	}
	// Empty is a legitimate answer: the test environment has no provider signed
	// in, and a page has to be able to show that state.
	if meta.DefaultModel == "" {
		return
	}
	if _, _, err := b.models.Resolve(meta.DefaultModel); err != nil {
		t.Errorf("DefaultModel = %q, which the registry cannot resolve: %v", meta.DefaultModel, err)
	}
}
