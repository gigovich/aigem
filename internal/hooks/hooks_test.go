package hooks

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMatcherMatches(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"", "bash", true},
		{"*", "bash", true},
		{"bash", "bash", true},
		{"bash", "grep", false},
		{"write_file|edit_file", "edit_file", true},
		{"write_file|edit_file", "bash", false},
		{"read_.*", "read_file", true},
		{"read_.*", "write_file", false},
	}
	for _, c := range cases {
		if got := matcherMatches(c.pattern, c.name); got != c.want {
			t.Errorf("matcherMatches(%q,%q)=%v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// runnerWith builds a Runner with a single matcher set for event, no settings IO.
func runnerWith(event string, matchers ...Matcher) *Runner {
	return &Runner{base: map[string][]Matcher{event: matchers}}
}

func cmdHook(script string) Hook { return Hook{Type: "command", Command: script} }

func TestRunNoHooks(t *testing.T) {
	r := &Runner{base: map[string][]Matcher{}}
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if dec.Block || dec.Allow || !dec.Continue {
		t.Fatalf("empty runner should allow and continue: %+v", dec)
	}
}

func TestRunDenyByExitCode(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Matcher: "bash", Hooks: []Hook{
		cmdHook(`echo "no bash allowed" >&2; exit 2`),
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if !dec.Block {
		t.Fatal("expected block from exit 2")
	}
	if dec.Reason != "no bash allowed" {
		t.Fatalf("expected stderr reason, got %q", dec.Reason)
	}
}

func TestRunAllowSkipsConfirm(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		cmdHook(`echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`),
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if !dec.Allow || dec.Block {
		t.Fatalf("expected allow, got %+v", dec)
	}
}

func TestRunDenyWinsOverAllow(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		cmdHook(`echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`),
		cmdHook(`echo '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"nope"}}'`),
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if !dec.Block || dec.Allow {
		t.Fatalf("deny should win, got %+v", dec)
	}
	if dec.Reason != "nope" {
		t.Fatalf("expected deny reason, got %q", dec.Reason)
	}
}

func TestRunUpdatedInputAndContext(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		cmdHook(`echo '{"hookSpecificOutput":{"updatedInput":{"path":"x"},"additionalContext":"hi"}}'`),
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "read_file"})
	if string(dec.UpdatedInput) != `{"path":"x"}` {
		t.Fatalf("updatedInput not parsed: %s", dec.UpdatedInput)
	}
	if dec.Context != "hi" {
		t.Fatalf("context not parsed: %q", dec.Context)
	}
}

func TestRunUpdatedOutput(t *testing.T) {
	r := runnerWith(EventPostToolUse, Matcher{Hooks: []Hook{
		cmdHook(`echo '{"hookSpecificOutput":{"updatedToolOutput":"replaced"}}'`),
	}})
	dec := r.Run(context.Background(), EventPostToolUse, Input{ToolName: "bash"})
	if dec.UpdatedOutput == nil || *dec.UpdatedOutput != "replaced" {
		t.Fatalf("updatedOutput not parsed: %v", dec.UpdatedOutput)
	}
}

func TestRunStdinReceivesInput(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		// Echo back additionalContext built from the tool name read off stdin.
		cmdHook(`name=$(cat | sed -n 's/.*"tool_name":"\([^"]*\)".*/\1/p'); ` +
			`printf '{"hookSpecificOutput":{"additionalContext":"saw:%s"}}' "$name"`),
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if dec.Context != "saw:bash" {
		t.Fatalf("hook did not receive stdin input, got %q", dec.Context)
	}
}

func TestRunNonBlockingStderrNotice(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		cmdHook(`echo "just a warning" >&2; exit 1`),
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if dec.Block {
		t.Fatal("exit 1 must not block")
	}
	if len(dec.Notices) != 1 || dec.Notices[0] != "just a warning" {
		t.Fatalf("expected non-blocking notice, got %v", dec.Notices)
	}
}

func TestRunTimeout(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		{Type: "command", Command: "sleep 5", Timeout: 1},
	}})
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	// A killed hook exits non-zero (not code 2), so it is non-blocking.
	if dec.Block {
		t.Fatal("timed-out hook should not block")
	}
}

func TestRunContinueFalse(t *testing.T) {
	r := runnerWith(EventStop, Matcher{Hooks: []Hook{
		cmdHook(`echo '{"continue":false,"stopReason":"done"}'`),
	}})
	dec := r.Run(context.Background(), EventStop, Input{})
	if dec.Continue || dec.StopReason != "done" {
		t.Fatalf("expected stop, got %+v", dec)
	}
}

func TestRunSessionTitle(t *testing.T) {
	r := runnerWith(EventSessionStart, Matcher{Hooks: []Hook{
		cmdHook(`echo '{"sessionTitle":"My Session"}'`),
	}})
	dec := r.Run(context.Background(), EventSessionStart, Input{Source: "startup"})
	if dec.SessionTitle != "My Session" {
		t.Fatalf("sessionTitle not parsed, got %q", dec.SessionTitle)
	}
}

func TestForSessionKeepsConcurrentIdentities(t *testing.T) {
	matchers := []Matcher{{Matcher: "*", Hooks: []Hook{
		cmdHook(`python3 -c 'import json,sys; x=json.load(sys.stdin); print(json.dumps({"hookSpecificOutput":{"additionalContext":"%s|%s|%s" % (x["session_id"],x["transcript_path"],x["cwd"])}}))'`),
	}}}
	r := &Runner{base: map[string][]Matcher{
		EventSessionStart:     matchers,
		EventUserPromptSubmit: matchers,
		EventSessionEnd:       matchers,
	}}
	one := r.ForSession("one", "/tmp/one.jsonl", "/one")
	two := r.ForSession("two", "/tmp/two.jsonl", "/two")

	// Both views stay alive while their events are interleaved. This is also a
	// negative control for the old shared SetSession design: that design would
	// make at least one of these calls observe the other view's identity.
	type result struct {
		want, got string
	}
	results := make(chan result, 48)
	var wg sync.WaitGroup
	for _, item := range []struct {
		runner *Runner
		want   string
	}{
		{one, "one|/tmp/one.jsonl|/one"},
		{two, "two|/tmp/two.jsonl|/two"},
	} {
		wg.Add(1)
		go func(item struct {
			runner *Runner
			want   string
		}) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				for _, event := range []string{EventSessionStart, EventUserPromptSubmit, EventSessionEnd} {
					dec := item.runner.Run(context.Background(), event, Input{})
					results <- result{want: item.want, got: dec.Context}
				}
			}
		}(item)
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got.got != got.want {
			t.Fatalf("hook identity changed between live sessions: got %q, want %q", got.got, got.want)
		}
	}
}

func TestRunCapsHugeOutput(t *testing.T) {
	// A hook that prints far more than maxHookOutput must complete promptly and
	// not block on a full pipe.
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		cmdHook(`head -c 5000000 /dev/zero | tr '\0' 'x'; exit 0`),
	}})
	done := make(chan struct{})
	go func() {
		r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hook with huge output hung")
	}
}

func TestRunKillsChildOnTimeout(t *testing.T) {
	// The shell waits on a child sleep; on timeout the whole process group is
	// killed, so Run returns near the timeout, not after the 30s sleep.
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{
		{Type: "command", Command: "sleep 30 & wait", Timeout: 1},
	}})
	start := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
		start <- time.Since(t0)
	}()
	select {
	case d := <-start:
		if d > 8*time.Second {
			t.Fatalf("timed-out hook took too long to return: %v", d)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("timed-out hook did not return (child not killed)")
	}
}

func TestLoadMergesSettings(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	cfg := `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"true"}]}]}}`
	mustWrite(t, filepath.Join(dir, ".aigem", "settings.json"), cfg)

	r, warns := Load(dir)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	// Project-local hooks are withheld until the project dir is trusted.
	if !r.HasUntrustedProjectHooks() {
		t.Fatal("expected project-local hooks to be flagged untrusted")
	}
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 0 {
		t.Fatalf("untrusted project hooks must not match, got %d", got)
	}
	r.trusted = true
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 1 {
		t.Fatalf("expected 1 matching hook once trusted, got %d", got)
	}
	if got := len(r.matching(EventPreToolUse, "grep", nil)); got != 0 {
		t.Fatalf("expected 0 for grep, got %d", got)
	}
}

func TestProjectHooksGatedOnTrust(t *testing.T) {
	mk := func() []Matcher {
		return []Matcher{{Matcher: "*", Hooks: []Hook{cmdHook("true")}}}
	}
	r := &Runner{
		base:       map[string][]Matcher{EventPreToolUse: mk()},
		project:    map[string][]Matcher{EventPreToolUse: mk()},
		hasProject: true,
	}
	// Untrusted: only the global (base) hook matches.
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 1 {
		t.Fatalf("untrusted should match only the global hook, got %d", got)
	}
	if !r.HasUntrustedProjectHooks() {
		t.Fatal("expected untrusted project hooks to be flagged")
	}
	r.trusted = true
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 2 {
		t.Fatalf("trusted should match global + project, got %d", got)
	}
	if r.HasUntrustedProjectHooks() {
		t.Fatal("trusted project should no longer be flagged")
	}
}

func TestDisableAllHooks(t *testing.T) {
	r := runnerWith(EventPreToolUse, Matcher{Hooks: []Hook{cmdHook("exit 2")}})
	r.disabled = true
	dec := r.Run(context.Background(), EventPreToolUse, Input{ToolName: "bash"})
	if dec.Block {
		t.Fatal("disabled runner must not run hooks")
	}
}

func TestProjectDisableAllHooksDoesNotDisableGlobalHooks(t *testing.T) {
	mk := func() []Matcher {
		return []Matcher{{Matcher: "*", Hooks: []Hook{cmdHook("true")}}}
	}
	r := &Runner{
		base:            map[string][]Matcher{EventPreToolUse: mk()},
		project:         map[string][]Matcher{EventPreToolUse: mk()},
		hasProject:      true,
		projectDisabled: true,
	}
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 1 {
		t.Fatalf("untrusted project disableAllHooks must not suppress global hooks, got %d", got)
	}
	r.trusted = true
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 1 {
		t.Fatalf("trusted project disableAllHooks should suppress only project hooks, got %d", got)
	}
}

func TestFromAnyAndScoped(t *testing.T) {
	m := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "bash",
				"hooks":   []any{map[string]any{"type": "command", "command": "true"}},
			},
		},
	}
	cfg, err := FromAny(m)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{base: map[string][]Matcher{}}
	if got := len(r.matching(EventPreToolUse, "bash", cfg)); got != 1 {
		t.Fatalf("scoped hook not matched, got %d", got)
	}
	if got := len(r.matching(EventPreToolUse, "bash", nil)); got != 0 {
		t.Fatalf("no scoped hooks should match without the map, got %d", got)
	}
}

func TestProjectHookApprovalInvalidatesOnConfigChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, ".aigem", "settings.json")
	mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, path, `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"true"}]}]}}`)
	r, warns := Load(dir)
	if len(warns) != 0 || !r.HasUntrustedProjectHooks() {
		t.Fatalf("initial hook state: untrusted=%v warnings=%v", r.HasUntrustedProjectHooks(), warns)
	}
	if err := r.TrustProject(); err != nil {
		t.Fatal(err)
	}
	r, warns = Load(dir)
	if len(warns) != 0 || r.HasUntrustedProjectHooks() {
		t.Fatalf("approved hook state: untrusted=%v warnings=%v", r.HasUntrustedProjectHooks(), warns)
	}
	mustWrite(t, path, `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"echo changed"}]}]}}`)
	r, warns = Load(dir)
	if len(warns) != 0 || !r.HasUntrustedProjectHooks() {
		t.Fatalf("changed hook approval was not invalidated: untrusted=%v warnings=%v", r.HasUntrustedProjectHooks(), warns)
	}
}

func TestLoadWarnsOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".aigem", "settings.json"),
		`{"hooks":{"PreToolUse":[{"matcher":"(unclosed","hooks":[{"type":"http","command":""}]}]}}`)
	_, warns := Load(dir)
	if len(warns) < 2 {
		t.Fatalf("expected warnings for bad regexp, unsupported type, and empty command, got %v", warns)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
