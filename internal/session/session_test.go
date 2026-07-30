package session

import (
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
)

func TestSaveListLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	s := &Session{
		Meta:     Meta{ID: NewID(now), Title: Title("hello world"), Created: now, Model: "openai/gpt-5.6-sol"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello world"}},
	}
	if err := Save(s, now); err != nil {
		t.Fatal(err)
	}

	metas, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Title != "hello world" {
		t.Fatalf("unexpected metas: %+v", metas)
	}
	if metas[0].Model != "openai/gpt-5.6-sol" {
		t.Fatalf("model not persisted in meta: %q", metas[0].Model)
	}

	loaded, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello world" {
		t.Fatalf("unexpected messages: %+v", loaded.Messages)
	}
}

func TestTitle(t *testing.T) {
	if got := Title("ab\ncd"); got != "ab cd" {
		t.Fatalf("newline not flattened: %q", got)
	}
	if Title("") != "(untitled)" {
		t.Fatal("empty title should be (untitled)")
	}
}
