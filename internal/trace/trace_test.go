package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gigovich/aigem/internal/agent"
)

func TestRecordsDelegationStructure(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)

	r.Start("review both services", "test-model")
	ev := r.Wrap(agent.Events{})
	ev.OnToolBatch(3, []agent.ToolCallRef{{ID: "call-a", Name: "task"}, {ID: "call-b", Name: "task"}})
	ev.OnAgentStart("call-1", "scout", "explore services/alpha")
	ev.OnSubToolStart("call-1", "scout", "grep", json.RawMessage(`{"pattern":"x"}`))
	ev.OnSubToolEnd("call-1", "scout", "grep", "hit", nil)
	ev.OnAgentEnd("call-1", "alpha exposes /v1", nil)
	r.End("done", nil)

	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	want := []string{KindRunStart, KindToolBatch, KindAgentStart, KindSubToolStart,
		KindSubToolEnd, KindAgentEnd, KindRunEnd}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i, e := range events {
		if e.Seq != i+1 {
			t.Fatalf("event %d has seq %d; sequence numbers must be dense and ordered", i, e.Seq)
		}
	}
	if len(events[1].Calls) != 2 || events[1].Calls[0].ID != "call-a" {
		t.Fatalf("batch lost its grouping or its ids: %v", events[1].Calls)
	}
	// Events after a batch inherit its round, so a delegation can be tied back to
	// the assistant message that issued it.
	for _, e := range events[1:6] {
		if e.Round != 3 {
			t.Fatalf("%s recorded round %d, want 3", e.Kind, e.Round)
		}
	}
	// The delegated prompt is kept whole - it is what self-containedness is
	// scored on.
	if events[2].Text != "explore services/alpha" || events[2].Agent != "scout" {
		t.Fatalf("agent_start lost its prompt or agent: %+v", events[2])
	}
}

// Every callback Wrap replaces must still reach the front-end. Dropping a
// passthrough would silently blank part of the TUI while the trace looked fine.
func TestWrapKeepsOriginalCallbacks(t *testing.T) {
	var buf bytes.Buffer
	var seen []string
	base := agent.Events{
		OnToolBatch:       func(int, []agent.ToolCallRef) { seen = append(seen, "batch") },
		OnToolStart:       func(name string, _ json.RawMessage) { seen = append(seen, "start:"+name) },
		OnToolEnd:         func(name, _ string, _ error) { seen = append(seen, "end:"+name) },
		OnAgentStart:      func(_, name, _ string) { seen = append(seen, "agent:"+name) },
		OnAgentEnd:        func(_, _ string, _ error) { seen = append(seen, "agentend") },
		OnSubToolStart:    func(_, _, tool string, _ json.RawMessage) { seen = append(seen, "substart:"+tool) },
		OnSubToolEnd:      func(_, _, tool, _ string, _ error) { seen = append(seen, "subend:"+tool) },
		OnSubNotice:       func(_, _, text string) { seen = append(seen, "subnotice:"+text) },
		OnNotice:          func(text string) { seen = append(seen, "notice:"+text) },
		OnUsage:           func(tokens int) { seen = append(seen, "usage") },
		OnBudgetExhausted: func(reason string) { seen = append(seen, "budget") },
	}
	ev := NewWriter(&buf).Wrap(base)
	ev.OnToolBatch(1, []agent.ToolCallRef{{ID: "call-a", Name: "task"}})
	ev.OnToolStart("grep", nil)
	ev.OnToolEnd("grep", "out", nil)
	ev.OnAgentStart("id", "scout", "p")
	ev.OnSubToolStart("id", "scout", "read_file", nil)
	ev.OnSubToolEnd("id", "scout", "read_file", "body", nil)
	ev.OnSubNotice("id", "scout", "slow")
	ev.OnAgentEnd("id", "done", nil)
	ev.OnNotice("hi")
	ev.OnUsage(10)
	ev.OnBudgetExhausted("rounds")

	got := strings.Join(seen, ",")
	want := "batch,start:grep,end:grep,agent:scout,substart:read_file,subend:read_file," +
		"subnotice:slow,agentend,notice:hi,usage,budget"
	if got != want {
		t.Fatalf("front-end callbacks = %q, want %q", got, want)
	}
	if events, err := Parse(&buf); err != nil || len(events) != 11 {
		t.Fatalf("recorded %d events (err %v), want 11", len(events), err)
	}
}

// A disabled trace must not force every call site into a nil check.
func TestNilRecorderIsInert(t *testing.T) {
	var r *Recorder
	r.Start("p", "m")
	r.End("a", errors.New("boom"))
	if err := r.Close(); err != nil {
		t.Fatalf("Close on nil Recorder: %v", err)
	}
	base := agent.Events{}
	if ev := r.Wrap(base); ev.OnToolStart != nil {
		t.Fatal("nil Recorder must return the events untouched")
	}
}

func TestRecordsErrors(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	ev := r.Wrap(agent.Events{})
	ev.OnToolEnd("bash", "", errors.New("exit 1"))
	r.End("", errors.New("run failed"))

	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Error != "exit 1" || events[1].Error != "run failed" {
		t.Fatalf("errors not recorded: %+v", events)
	}
}

func TestClipTruncatesOnRuneBoundary(t *testing.T) {
	// A 3-byte rune against a cap that is not a multiple of 3, so the cut lands
	// mid-rune and the boundary walk is actually exercised.
	long := strings.Repeat("€", resultCap)
	if resultCap%3 == 0 {
		t.Fatalf("resultCap %d is a multiple of the rune width; this test proves nothing", resultCap)
	}
	got := clip(long)
	if !strings.HasSuffix(got, "...") {
		t.Fatal("long result was not truncated")
	}
	body := strings.TrimSuffix(got, "...")
	if len(body) >= len(long) {
		t.Fatalf("clip did not shorten the string: %d bytes", len(body))
	}
	// json.Valid would accept a split rune; only utf8 catches it.
	if !utf8.ValidString(body) {
		t.Fatalf("clip split a multi-byte rune: %q", body)
	}
	if short := "ok"; clip(short) != short {
		t.Fatalf("clip altered a short result: %q", clip(short))
	}
}

// The prompt and the answer are kept long but not unbounded: one over-long line
// would make the trace unreadable by its own parser.
func TestPromptAndAnswerAreCappedButLong(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	huge := strings.Repeat("a", textCap*3)
	r.Start(huge, "m")
	r.End(huge, nil)

	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if len(e.Text) > textCap+8 {
			t.Fatalf("%s kept %d bytes of text", e.Kind, len(e.Text))
		}
		if e.Bytes != len(huge) {
			t.Fatalf("%s reported %d bytes, want the untruncated %d", e.Kind, e.Bytes, len(huge))
		}
	}
}

// Subagents run concurrently, so their events arrive from several goroutines.
// A torn line would make the trace unparseable.
func TestConcurrentWritesStayWellFormed(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	ev := r.Wrap(agent.Events{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev.OnSubToolStart("id", "scout", "grep", json.RawMessage(`{"a":1}`))
			ev.OnSubToolEnd("id", "scout", "grep", strings.Repeat("x", 1000), nil)
		}()
	}
	wg.Wait()

	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 40 {
		t.Fatalf("got %d events, want 40", len(events))
	}
	for _, e := range events {
		if e.Kind == KindSubToolEnd && e.Bytes != 1000 {
			t.Fatalf("Bytes must report the untruncated size, got %d", e.Bytes)
		}
	}
}

func TestCreateWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Start("p", "m")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := Parse(bytes.NewReader(data))
	if err != nil || len(events) != 1 || events[0].Model != "m" {
		t.Fatalf("round trip failed: %v %+v", err, events)
	}
}

// The trace holds the prompt, the answer, and tool arguments verbatim - the
// same class of content as the credential store, which is 0600.
func TestCreatedFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("trace file mode is %v, want 0600", mode)
	}
}

// O_CREATE's mode applies only when the file is new, so a path that already
// exists at 0644 would otherwise keep it.
func TestCreateTightensAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("pre-existing trace file kept mode %v, want 0600", mode)
	}
}

// A write_file call carries the whole new file in its arguments, and a bash
// call carries a command line that may hold a token. Neither belongs in the
// trace at full length, and an oversized line also makes Parse fail.
func TestLargeArgumentsAreClipped(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	ev := r.Wrap(agent.Events{})
	big := json.RawMessage(`{"content":"` + strings.Repeat("x", 5000) + `"}`)
	ev.OnToolStart("write_file", big)
	ev.OnSubToolStart("id", "code-writer", "write_file", big)

	if buf.Len() > 4*resultCap {
		t.Fatalf("trace grew to %d bytes; arguments were not clipped", buf.Len())
	}
	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if len(e.Args) > resultCap+16 {
			t.Fatalf("%s kept %d bytes of arguments", e.Kind, len(e.Args))
		}
	}
}

// Arguments are raw model output and are not guaranteed to be valid JSON.
// Dropping the event would leave a hole in the sequence and hide the very call
// that misbehaved.
func TestMalformedArgumentsStillRecordTheEvent(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	ev := r.Wrap(agent.Events{})
	ev.OnToolStart("bash", json.RawMessage(`{"cmd": "ls`))
	ev.OnToolEnd("bash", "ok", nil)

	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 - a malformed argument dropped one", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("sequence has a hole: %d, %d", events[0].Seq, events[1].Seq)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("a recoverable argument problem must not latch an error: %v", err)
	}
}

func TestSubagentNoticesAreRecorded(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	ev := r.Wrap(agent.Events{})
	ev.OnSubNotice("id", "code-writer", "tool-call budget exhausted")

	events, err := Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != KindSubNotice || events[0].Agent != "code-writer" {
		t.Fatalf("subagent notice not recorded: %+v", events)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A truncated trace reads as a run that simply did less, so the write error has
// to reach the caller.
func TestWriteErrorSurfacesOnClose(t *testing.T) {
	r := NewWriter(failingWriter{})
	r.Start("p", "m")
	if err := r.Close(); err == nil {
		t.Fatal("Close must report the write failure")
	}
}

func TestParseRejectsMalformedLine(t *testing.T) {
	if _, err := Parse(strings.NewReader("{\"kind\":\"run_start\"}\nnot json\n")); err == nil {
		t.Fatal("a malformed line must fail the read, not be skipped")
	}
}
