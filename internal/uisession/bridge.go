package uisession

import (
	"encoding/json"

	"github.com/gigovich/aigem/internal/agent"
)

// Bridge turns the agent's callbacks into events on the stream emit writes to.
//
// It is package-level rather than a method because there is a second thing that
// runs agents and wants the same timeline: an unattended bot, whose steps
// otherwise reach only the log. Both feed the same event vocabulary, so one
// front-end renders either.
func Bridge(emit func(Event)) agent.Events {
	return agent.Events{
		OnContent:          func(d string) { emit(Event{Kind: KindContent, Text: d}) },
		OnReasoning:        func(d string) { emit(Event{Kind: KindReasoning, Text: d}) },
		OnAssistantMessage: func(c string) { emit(Event{Kind: KindAssistantMessage, Text: c}) },
		OnNotice:           func(t string) { emit(Event{Kind: KindNotice, Text: t}) },
		OnUsage:            func(n int) { emit(Event{Kind: KindUsage, Tokens: n}) },
		OnTodoUpdate:       func(td []agent.TodoItem) { emit(Event{Kind: KindTodo, Todos: td}) },
		OnBudgetExhausted:  func(r string) { emit(Event{Kind: KindBudgetExhausted, Text: r}) },
		OnToolBatch: func(round int, calls []agent.ToolCallRef) {
			out := make([]Call, len(calls))
			for i, c := range calls {
				out[i] = Call{ID: c.ID, Name: c.Name}
			}
			emit(Event{Kind: KindToolBatch, Round: round, Calls: out})
		},
		OnToolStart: func(id, name string, args json.RawMessage) {
			emit(Event{Kind: KindToolStart, ID: id, Name: name, Args: args})
		},
		OnToolEnd: func(id, name, result string, err error) {
			emit(Event{Kind: KindToolEnd, ID: id, Name: name, Text: result, Error: errText(err)})
		},
		OnAgentStart: func(id, name, prompt string) {
			emit(Event{Kind: KindAgentStart, ID: id, Agent: name, Text: prompt})
		},
		OnAgentEnd: func(id, result string, err error) {
			emit(Event{Kind: KindAgentEnd, ID: id, Text: result, Error: errText(err)})
		},
		OnSubToolStart: func(runID, name, callID, tool string, args json.RawMessage) {
			emit(Event{Kind: KindSubToolStart, RunID: runID, Agent: name,
				ID: callID, Name: tool, Args: args})
		},
		OnSubToolEnd: func(runID, name, callID, tool, result string, err error) {
			emit(Event{Kind: KindSubToolEnd, RunID: runID, Agent: name,
				ID: callID, Name: tool, Text: result, Error: errText(err)})
		},
		OnSubNotice: func(runID, name, text string) {
			emit(Event{Kind: KindSubNotice, RunID: runID, Agent: name, Text: text})
		},
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
