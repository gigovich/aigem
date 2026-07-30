package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// TodoToolName is the tool the model calls to maintain its working plan.
const TodoToolName = "todo_write"

// maxTodos bounds the plan so a runaway model cannot inflate the sidebar or the
// context echo without limit.
const maxTodos = 100

// todoTool lets the model record and update a plan as a list of items. The whole
// list is replaced on every call (the model resends the full plan with updated
// statuses), which keeps the latest plan in context and drives the UI sidebar.
type todoTool struct{ a *Agent }

// NewTodoTool builds the plan tool bound to agent a. Register it into the agent's
// registry so the model can call it.
func NewTodoTool(a *Agent) tools.Tool { return &todoTool{a: a} }

func (t *todoTool) Name() string       { return TodoToolName }
func (t *todoTool) NeedsConfirm() bool { return false }

func (t *todoTool) Description() string {
	return "Maintain a working plan for a multi-step task. Call this with the full list of " +
		"steps before starting work, then call it again to update statuses as you go - always " +
		"resend the entire list. Mark exactly one item in_progress while you work on it, and " +
		"completed once it is done. Skip this for trivial single-step requests."
}

func (t *todoTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"todos":{
				"type":"array",
				"description":"The full plan, in order. Resend every item on each call.",
				"items":{
					"type":"object",
					"properties":{
						"text":{"type":"string","description":"Short imperative description of the step."},
						"status":{"type":"string","enum":["pending","in_progress","completed"]}
					},
					"required":["text","status"]
				}
			}
		},
		"required":["todos"]
	}`)
}

func (t *todoTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid todos: %w", err)
	}
	cleaned := make([]TodoItem, 0, len(in.Todos))
	for _, it := range in.Todos {
		text := strings.TrimSpace(it.Text)
		if text == "" {
			continue
		}
		status := strings.TrimSpace(strings.ToLower(it.Status))
		switch status {
		case TodoPending, TodoInProgress, TodoCompleted:
		default:
			status = TodoPending
		}
		cleaned = append(cleaned, TodoItem{Text: text, Status: status})
		if len(cleaned) >= maxTodos {
			break
		}
	}
	t.a.setTodos(cleaned)
	return summarizePlan(cleaned), nil
}

// summarizePlan renders the plan as a compact text result so the latest plan is
// echoed back into the model's context on every update.
func summarizePlan(todos []TodoItem) string {
	if len(todos) == 0 {
		return "Plan cleared."
	}
	var b strings.Builder
	var done int
	for _, t := range todos {
		mark := "[ ]"
		switch t.Status {
		case TodoCompleted:
			mark = "[x]"
			done++
		case TodoInProgress:
			mark = "[~]"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, t.Text)
	}
	fmt.Fprintf(&b, "(%d/%d done)", done, len(todos))
	return b.String()
}
