package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

type memoryTool struct{ store *Store }

// NewMemoryTool exposes a bot's persistent memory to the model. It is not confirm-gated:
// the bot manages its own memory unattended.
func NewMemoryTool(store *Store) tools.Tool { return &memoryTool{store: store} }

func (t *memoryTool) Name() string       { return "memory" }
func (t *memoryTool) NeedsConfirm() bool { return false }

func (t *memoryTool) Description() string {
	return "Your persistent memory. The index of saved facts is in your system prompt. " +
		"Actions: save (add or replace a fact - give name, a one-line description, and content); " +
		"read (get a fact's full content by name); delete (remove a fact by name); list (show the index); " +
		"archive (reversibly retire a fact - it leaves the index but can be brought back with restore); " +
		"restore (bring an archived fact back); " +
		"audit (staleness overview: per-fact age, last use, use count, plus archived fact names); " +
		"inspect (read a fact, active or archived, without counting it as a use - for reviewing memory). " +
		"You own this memory: record durable facts, revise them when they change, delete what is false."
}

func (t *memoryTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["save","read","delete","list","archive","restore","audit","inspect"]},
			"name":{"type":"string","description":"fact name (required for save/read/delete/archive/restore/inspect)"},
			"description":{"type":"string","description":"one-line summary for the index (required for save)"},
			"content":{"type":"string","description":"the fact's full body (required for save)"}
		},
		"required":["action"]
	}`)
}

func (t *memoryTool) Run(_ context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Action      string `json:"action"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	switch a.Action {
	case "save":
		if a.Name == "" || a.Description == "" || strings.TrimSpace(a.Content) == "" {
			return "", fmt.Errorf("save requires name, description, and content")
		}
		if err := t.store.Save(a.Name, a.Description, a.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("saved memory %q", a.Name), nil
	case "read":
		if a.Name == "" {
			return "", fmt.Errorf("read requires name")
		}
		return t.store.Read(a.Name)
	case "delete":
		if a.Name == "" {
			return "", fmt.Errorf("delete requires name")
		}
		if err := t.store.Delete(a.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("deleted memory %q", a.Name), nil
	case "list":
		idx, err := t.store.Index()
		if err != nil {
			return "", err
		}
		if idx == "" {
			return "(memory is empty)", nil
		}
		return idx, nil
	case "archive":
		if a.Name == "" {
			return "", fmt.Errorf("archive requires name")
		}
		if err := t.store.Archive(a.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("archived memory %q (recoverable with restore)", a.Name), nil
	case "restore":
		if a.Name == "" {
			return "", fmt.Errorf("restore requires name")
		}
		if err := t.store.Restore(a.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("restored memory %q", a.Name), nil
	case "audit":
		return t.store.Audit()
	case "inspect":
		if a.Name == "" {
			return "", fmt.Errorf("inspect requires name")
		}
		return t.store.Inspect(a.Name)
	default:
		return "", fmt.Errorf(
			"unknown action %q; use save, read, delete, list, archive, restore, audit, or inspect", a.Action)
	}
}
