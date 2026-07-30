package agent

// TodoItem is one entry in the agent's working plan. Status is one of
// "pending", "in_progress", or "completed".
type TodoItem struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// Todo status values.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// Todos returns a copy of the current plan.
func (a *Agent) Todos() []TodoItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]TodoItem, len(a.todos))
	copy(out, a.todos)
	return out
}

// setTodos replaces the plan; callers pass items already validated by the tool.
func (a *Agent) setTodos(items []TodoItem) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.todos = items
}

// hasOpenPlan reports whether a plan exists with at least one item not yet
// completed - the condition that arms the autonomous evaluator.
func (a *Agent) hasOpenPlan() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.todos {
		if t.Status != TodoCompleted {
			return true
		}
	}
	return false
}

// clearTodosIfComplete drops the plan when it exists and every item is completed,
// so a finished plan does not linger in the sidebar into the next user turn. A
// partially-done plan is kept (the new turn may be a follow-up). Returns whether
// it cleared.
func (a *Agent) clearTodosIfComplete() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.todos) == 0 {
		return false
	}
	for _, t := range a.todos {
		if t.Status != TodoCompleted {
			return false
		}
	}
	a.todos = nil
	return true
}

// completeOpenTodos closes every still-open step (pending and in_progress) when
// the evaluator reports the plan's work done but the model forgot the final
// todo_write. The evaluator's "done" verdict is authoritative, so a lingering
// pending or in_progress step is treated as a missed mark, not abandoned work:
// a model with loose todo discipline otherwise leaves the sidebar stuck (e.g.
// 0/5) even though it finished. Returns whether anything changed.
func (a *Agent) completeOpenTodos() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := false
	for i := range a.todos {
		if a.todos[i].Status != TodoCompleted {
			a.todos[i].Status = TodoCompleted
			changed = true
		}
	}
	return changed
}

// nextOpen returns the text of the first in-progress (then pending) todo, or "".
func (a *Agent) nextOpen() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.todos {
		if t.Status == TodoInProgress {
			return t.Text
		}
	}
	for _, t := range a.todos {
		if t.Status == TodoPending {
			return t.Text
		}
	}
	return ""
}
