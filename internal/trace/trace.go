// Package trace records an agent run as JSONL so a run can be scored offline.
// The human-readable activity a front-end prints is for reading, not parsing:
// it truncates, interleaves, and loses which calls the model batched together.
// A trace keeps the structure the eval harness needs.
package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"unicode/utf8"

	"github.com/gigovich/aigem/internal/agent"
)

// Event kinds. One JSON object per line, in occurrence order.
const (
	KindRunStart     = "run_start"
	KindRunEnd       = "run_end"
	KindToolBatch    = "tool_batch"
	KindToolStart    = "tool_start"
	KindToolEnd      = "tool_end"
	KindAgentStart   = "agent_start"
	KindAgentEnd     = "agent_end"
	KindSubToolStart = "sub_tool_start"
	KindSubToolEnd   = "sub_tool_end"
	KindSubNotice    = "sub_notice"
	KindNotice       = "notice"
	KindUsage        = "usage"
	KindBudget       = "budget_exhausted"
)

// resultCap bounds how much of a tool result - or of a tool's arguments - is
// stored. Scoring cares that a call happened and whether it failed, not what it
// read or wrote; Bytes keeps the real size so a truncated field is never
// mistaken for a small one. The cap also keeps two hazards out of the file: a
// whole write_file body or bash command line preserved verbatim, and a line so
// long that reading the trace back fails.
const resultCap = 400

// textCap bounds the prompt and the final answer. They are worth keeping at
// length - they are what a reader compares the delegations against - but not at
// unbounded length, since one over-long line makes the whole trace unparseable.
const textCap = 8000

// Call is one tool call inside a batch.
type Call struct {
	ID   string `json:"id,omitempty"`
	Tool string `json:"tool"`
}

// Event is one recorded step. Fields are omitted when they do not apply, so a
// line stays readable by eye as well as by the scorer.
type Event struct {
	Seq   int    `json:"seq"`
	Kind  string `json:"kind"`
	Round int    `json:"round,omitempty"`
	// ID is the parent task call's id, present on every event belonging to a
	// delegated run so concurrent subagents can be told apart.
	ID    string `json:"id,omitempty"`
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	Tool  string `json:"tool,omitempty"`
	// Calls lists a batch's calls with their ids. The ids are what tie a nested
	// run back to the call that started it: a delegated subagent and a forked
	// skill announce themselves identically, and only the id says which is
	// which.
	Calls  []Call          `json:"calls,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Text   string          `json:"text,omitempty"`
	Bytes  int             `json:"bytes,omitempty"`
	Tokens int             `json:"tokens,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Recorder writes events to a stream. Subagents run concurrently, so every
// write goes through the mutex.
type Recorder struct {
	mu    sync.Mutex
	w     io.Writer
	c     io.Closer
	seq   int
	round int
	err   error
}

// NewWriter records to w. The caller owns w.
func NewWriter(w io.Writer) *Recorder { return &Recorder{w: w} }

// Create records to a new file at path, truncating any existing one. The file
// is 0600: it holds the prompt, the answer, and tool arguments verbatim, which
// is the same class of content as the credential and session stores. The Chmod
// covers a path that already existed, where the OpenFile mode is ignored.
func Create(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return &Recorder{w: f, c: f}, nil
}

// Close closes the underlying file, if this Recorder opened one, and reports
// the first write error seen. A nil Recorder closes cleanly, so a caller that
// tracing is disabled for needs no branch.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c != nil {
		if err := r.c.Close(); err != nil && r.err == nil {
			r.err = err
		}
		r.c = nil
	}
	return r.err
}

func (r *Recorder) emit(e Event) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if e.Round == 0 {
		e.Round = r.round
	}
	line, err := json.Marshal(e)
	if err != nil {
		// Tool arguments arrive as raw model output and are not guaranteed to be
		// valid JSON. Dropping the event would leave a hole in the sequence and
		// hide the very call that misbehaved, so it is re-emitted with the
		// arguments demoted to a plain string.
		e.Args = quoted(e.Args)
		if line, err = json.Marshal(e); err != nil {
			if r.err == nil {
				r.err = err
			}
			return
		}
	}
	if _, err := fmt.Fprintf(r.w, "%s\n", line); err != nil && r.err == nil {
		r.err = err
	}
}

// Start records the run's opening event, prompt included: comparing prompts to
// what the model then delegated is the point of the file.
func (r *Recorder) Start(prompt, model string) {
	r.emit(Event{Kind: KindRunStart, Text: clipTo(prompt, textCap), Bytes: len(prompt), Model: model})
}

// End records the final answer and how the run terminated.
func (r *Recorder) End(answer string, err error) {
	e := Event{Kind: KindRunEnd, Text: clipTo(answer, textCap), Bytes: len(answer)}
	if err != nil {
		e.Error = err.Error()
	}
	r.emit(e)
}

// Wrap returns ev with a recording callback chained onto each hook. The
// original callbacks still run, so a front-end keeps its own output. A nil
// Recorder returns ev untouched.
//
// Streaming text (OnContent, OnReasoning, OnAssistantMessage) and the plan
// (OnTodoUpdate) are deliberately not recorded: the final answer is captured by
// End, and the rest is the same content arriving token by token.
func (r *Recorder) Wrap(ev agent.Events) agent.Events {
	if r == nil {
		return ev
	}
	out := ev

	out.OnToolBatch = func(round int, calls []agent.ToolCallRef) {
		r.mu.Lock()
		r.round = round
		r.mu.Unlock()
		recorded := make([]Call, len(calls))
		for i, c := range calls {
			recorded[i] = Call{ID: c.ID, Tool: c.Name}
		}
		r.emit(Event{Kind: KindToolBatch, Round: round, Calls: recorded})
		if ev.OnToolBatch != nil {
			ev.OnToolBatch(round, calls)
		}
	}
	out.OnToolStart = func(name string, args json.RawMessage) {
		r.emit(Event{Kind: KindToolStart, Tool: name, Args: clipArgs(args)})
		if ev.OnToolStart != nil {
			ev.OnToolStart(name, args)
		}
	}
	out.OnToolEnd = func(name, result string, err error) {
		r.emit(Event{Kind: KindToolEnd, Tool: name, Text: clip(result), Bytes: len(result), Error: errText(err)})
		if ev.OnToolEnd != nil {
			ev.OnToolEnd(name, result, err)
		}
	}
	// A delegated prompt is kept whole: whether it stands on its own without the
	// parent conversation is the thing being measured.
	out.OnAgentStart = func(id, name, prompt string) {
		r.emit(Event{Kind: KindAgentStart, ID: id, Agent: name, Text: prompt})
		if ev.OnAgentStart != nil {
			ev.OnAgentStart(id, name, prompt)
		}
	}
	out.OnAgentEnd = func(id, result string, err error) {
		r.emit(Event{Kind: KindAgentEnd, ID: id, Text: clip(result), Bytes: len(result), Error: errText(err)})
		if ev.OnAgentEnd != nil {
			ev.OnAgentEnd(id, result, err)
		}
	}
	out.OnSubToolStart = func(id, name, tool string, args json.RawMessage) {
		r.emit(Event{Kind: KindSubToolStart, ID: id, Agent: name, Tool: tool, Args: clipArgs(args)})
		if ev.OnSubToolStart != nil {
			ev.OnSubToolStart(id, name, tool, args)
		}
	}
	out.OnSubNotice = func(id, name, text string) {
		r.emit(Event{Kind: KindSubNotice, ID: id, Agent: name, Text: text})
		if ev.OnSubNotice != nil {
			ev.OnSubNotice(id, name, text)
		}
	}
	out.OnSubToolEnd = func(id, name, tool, result string, err error) {
		r.emit(Event{Kind: KindSubToolEnd, ID: id, Agent: name, Tool: tool,
			Text: clip(result), Bytes: len(result), Error: errText(err)})
		if ev.OnSubToolEnd != nil {
			ev.OnSubToolEnd(id, name, tool, result, err)
		}
	}
	out.OnNotice = func(text string) {
		r.emit(Event{Kind: KindNotice, Text: text})
		if ev.OnNotice != nil {
			ev.OnNotice(text)
		}
	}
	out.OnUsage = func(tokens int) {
		r.emit(Event{Kind: KindUsage, Tokens: tokens})
		if ev.OnUsage != nil {
			ev.OnUsage(tokens)
		}
	}
	out.OnBudgetExhausted = func(reason string) {
		r.emit(Event{Kind: KindBudget, Text: reason})
		if ev.OnBudgetExhausted != nil {
			ev.OnBudgetExhausted(reason)
		}
	}
	return out
}

// Parse reads a recorded trace back. A malformed line fails the whole read:
// silently skipping one would understate exactly the activity being scored.
func Parse(rd io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(rd)
	// Every text field is capped on the way in, so this ceiling is headroom for a
	// pathological event rather than a limit anything normal approaches.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []Event
	for line := 1; sc.Scan(); line++ {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// clipArgs bounds a tool's arguments the same way results are bounded. Over the
// cap the JSON is replaced by a quoted excerpt rather than truncated in place,
// which would leave the field unparseable.
func clipArgs(args json.RawMessage) json.RawMessage {
	if len(args) <= resultCap {
		return args
	}
	return quoted(args)
}

// quoted renders bytes as a JSON string, clipped, so they can be carried in a
// json.RawMessage field whatever they contain.
func quoted(b []byte) json.RawMessage {
	out, err := json.Marshal(clip(string(b)))
	if err != nil {
		return json.RawMessage(`"(unrecordable arguments)"`)
	}
	return out
}

func clip(s string) string { return clipTo(s, resultCap) }

// clipTo truncates on a rune boundary, so a cut multi-byte character does not
// land in the trace as a replacement char.
func clipTo(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
