package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func run(t *testing.T, r *Registry, name string, args map[string]any) (string, error) {
	t.Helper()
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	raw, _ := json.Marshal(args)
	return tool.Run(context.Background(), raw)
}

func TestWriteReadListGrepFuzzy(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, r, "write_file", map[string]any{
		"path": "sub/hello.go", "content": "package main\n// needle here\n",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, r, "read_file", map[string]any{"path": "sub/hello.go"})
	if err != nil || !strings.Contains(out, "needle here") {
		t.Fatalf("read_file = %q, err=%v", out, err)
	}

	out, _ = run(t, r, "list_dir", map[string]any{"path": "."})
	if !strings.Contains(out, "sub/") {
		t.Fatalf("list_dir missing sub/: %q", out)
	}

	out, _ = run(t, r, "grep", map[string]any{"pattern": "needle"})
	if !strings.Contains(out, "hello.go") {
		t.Fatalf("grep missed match: %q", out)
	}

	out, _ = run(t, r, "fuzzy_find", map[string]any{"query": "hello"})
	if !strings.Contains(out, "hello.go") {
		t.Fatalf("fuzzy_find missed file: %q", out)
	}
}

func TestGrepBoundsAndSkipsVendorDirs(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A real source match, a single huge line (minified-bundle shape), and a
	// match buried in a dependency dir grep must not descend into.
	mustWrite(t, dir, "src/app.go", "package main // needle\n")
	mustWrite(t, dir, "src/min.js", strings.Repeat("x", 5000)+"needle\n")
	mustWrite(t, dir, "node_modules/dep/index.js", "var needle = 1\n")

	// A match buried in agent-scratch / VCS hidden dirs must be skipped by default.
	mustWrite(t, dir, ".scratch/progress/log.txt", "TODO(mcp): needle in scratch\n")

	out, err := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "src/app.go") {
		t.Fatalf("grep missed the real match: %q", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Fatalf("grep descended into node_modules: %q", out)
	}
	if strings.Contains(out, ".scratch") {
		t.Fatalf("grep descended into a hidden dir: %q", out)
	}
	if !strings.Contains(out, "[line truncated]") {
		t.Fatalf("grep did not truncate the long line: %q", out)
	}
	if len(out) > maxGrepOutput+1024 {
		t.Fatalf("grep output not bounded: %d bytes", len(out))
	}
	// No matched line in the output may exceed the per-line cap (plus marker).
	for _, l := range strings.Split(out, "\n") {
		if len(l) > maxGrepLineLen+64 {
			t.Fatalf("matched line exceeds cap (%d): %q", len(l), l[:80])
		}
	}

	// The skip exempts the search root: pointing grep at the hidden dir searches it.
	out, _ = run(t, r, "grep", map[string]any{"pattern": "needle", "path": ".scratch"})
	if !strings.Contains(out, "log.txt") {
		t.Fatalf("grep rooted at a hidden dir should search it: %q", out)
	}
}

func TestSandboxEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, r, "read_file", map[string]any{"path": "../secret.txt"}); err == nil {
		t.Fatal("expected escape rejection for ../secret.txt")
	}
	if _, err := run(t, r, "read_file", map[string]any{"path": secret}); err == nil {
		t.Fatal("expected escape rejection for absolute path outside root")
	}
}

// outsideFixture builds a sandbox plus a readable file outside it.
func outsideFixture(t *testing.T) (r *Registry, outside string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	other := t.TempDir()
	outside = filepath.Join(other, "shared.txt")
	if err := os.WriteFile(outside, []byte("from outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r, outside
}

func TestOutsideRootAsksTheApprover(t *testing.T) {
	r, outside := outsideFixture(t)
	var asked []PathIntent
	r.SetPathApprover(func(_ string, intent PathIntent) PathDecision {
		asked = append(asked, intent)
		return PathAllowOnce
	})

	out, err := run(t, r, "read_file", map[string]any{"path": outside})
	if err != nil || !strings.Contains(out, "from outside") {
		t.Fatalf("approved read = %q, %v", out, err)
	}
	if len(asked) != 1 || asked[0].Tool != "read_file" || asked[0].Write {
		t.Fatalf("intent = %+v", asked)
	}

	// Allow-once is not remembered, so the next call asks again.
	if _, err := run(t, r, "read_file", map[string]any{"path": outside}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 {
		t.Fatalf("allow-once was remembered: %d asks", len(asked))
	}
}

func TestOutsideRootDeniedByApprover(t *testing.T) {
	r, outside := outsideFixture(t)
	r.SetPathApprover(func(string, PathIntent) PathDecision { return PathDeny })
	if _, err := run(t, r, "read_file", map[string]any{"path": outside}); err == nil {
		t.Fatal("expected a denied path to fail")
	}
}

func TestOutsideRootWriteIntentIsFlagged(t *testing.T) {
	r, outside := outsideFixture(t)
	var got PathIntent
	r.SetPathApprover(func(_ string, intent PathIntent) PathDecision {
		got = intent
		return PathDeny
	})
	if _, err := run(t, r, "write_file", map[string]any{"path": outside, "content": "x"}); err == nil {
		t.Fatal("expected the write to be refused")
	}
	if !got.Write || got.Tool != "write_file" {
		t.Fatalf("intent = %+v, want a flagged write", got)
	}
}

func TestGrantedDirIsReadWithoutAsking(t *testing.T) {
	r, outside := outsideFixture(t)
	r.SetPathGrants(true)
	asks := 0
	r.SetPathApprover(func(string, PathIntent) PathDecision {
		asks++
		return PathAllowDir
	})
	if _, err := run(t, r, "read_file", map[string]any{"path": outside}); err != nil {
		t.Fatal(err)
	}
	// A sibling in the granted directory needs no second question.
	sibling := filepath.Join(filepath.Dir(outside), "other.txt")
	if err := os.WriteFile(sibling, []byte("sibling"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, r, "read_file", map[string]any{"path": sibling})
	if err != nil || !strings.Contains(out, "sibling") {
		t.Fatalf("sibling read = %q, %v", out, err)
	}
	if asks != 1 {
		t.Fatalf("asks = %d, want the grant to answer the second call", asks)
	}

	// A grant covers reads only: writing into the same directory still asks.
	if _, err := run(t, r, "write_file", map[string]any{"path": sibling, "content": "x"}); err != nil {
		t.Fatal(err)
	}
	if asks != 2 {
		t.Fatalf("asks = %d, want the write to ask despite the grant", asks)
	}
}

func TestSubsetHasIndependentPathPolicy(t *testing.T) {
	parent, outside := outsideFixture(t)
	parent.SetPathGrants(true)
	parent.SetPathApprover(func(string, PathIntent) PathDecision { return PathAllowDir })
	if _, err := run(t, parent, "read_file", map[string]any{"path": outside}); err != nil {
		t.Fatal(err)
	}

	sub := parent.Subset([]string{"read_file"})
	sub.SetPathGrants(false)
	if _, err := run(t, sub, "read_file", map[string]any{"path": outside}); err == nil {
		t.Fatal("subset inherited the parent path grant")
	}

	asked := 0
	sub.SetPathApprover(func(string, PathIntent) PathDecision {
		asked++
		return PathAllowOnce
	})
	if _, err := run(t, sub, "read_file", map[string]any{"path": outside}); err != nil {
		t.Fatalf("subset approver was not used: %v", err)
	}
	if asked != 1 {
		t.Fatalf("subset approver calls = %d, want 1", asked)
	}

	// The subset's policy must not change the parent policy in either direction.
	if _, err := run(t, parent, "read_file", map[string]any{"path": outside}); err != nil {
		t.Fatalf("parent grant was changed by subset: %v", err)
	}
}

func TestGrantsIgnoredWhenNotEnabled(t *testing.T) {
	// A bot leaves grants off, so a directory a human approved for the same
	// working directory must not silently open for it.
	r, outside := outsideFixture(t)
	r.SetPathGrants(true)
	r.SetPathApprover(func(string, PathIntent) PathDecision { return PathAllowDir })
	if _, err := run(t, r, "read_file", map[string]any{"path": outside}); err != nil {
		t.Fatal(err)
	}

	bot, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, bot, "read_file", map[string]any{"path": outside}); err == nil {
		t.Fatal("a registry without grants or an approver must still refuse")
	}
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	orig := "line one\ntarget line\nline three\ntarget line\n"
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ambiguous match without replace_all is rejected (file untouched).
	if _, err := run(t, r, "edit_file", map[string]any{
		"path": "f.txt", "old_string": "target line", "new_string": "X",
	}); err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("expected ambiguous-match error, got %v", err)
	}

	// Unique match (with context) edits only that spot, preserving the rest.
	if _, err := run(t, r, "edit_file", map[string]any{
		"path": "f.txt", "old_string": "one\ntarget line", "new_string": "one\nEDITED",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	want := "line one\nEDITED\nline three\ntarget line\n"
	if string(got) != want {
		t.Fatalf("edit clobbered the file.\n got: %q\nwant: %q", got, want)
	}

	// Missing old_string is rejected with a helpful message.
	if _, err := run(t, r, "edit_file", map[string]any{
		"path": "f.txt", "old_string": "nope", "new_string": "x",
	}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestBashRespectsCancellation(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Get("bash")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the command must not run to completion

	start := time.Now()
	out, _ := tool.Run(ctx, json.RawMessage(`{"cmd":"sleep 30"}`))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("bash ignored cancellation, ran %s: %q", elapsed, out)
	}
}

// Cancelling kills bash, but a child it backgrounded still holds the output
// pipe, and CombinedOutput waits for that pipe to close - measured at the full
// 30 seconds for a `sleep 30 &`, and unbounded for a dev server. The turn the
// caller thinks it cancelled runs on for as long as the orphan lives, and
// closing the session waits for that turn.
func TestBashDoesNotWaitForABackgroundedChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no bash")
	}
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Get("bash")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = tool.Run(ctx, json.RawMessage(`{"cmd":"sleep 60 & echo started; sleep 30"}`))
		done <- time.Since(start)
	}()
	// Long enough for bash to have started the child and gone to sleep.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case elapsed := <-done:
		// The command's own WaitDelay is 2s; anything near the child's lifetime
		// means the orphan was waited for.
		if elapsed > 10*time.Second {
			t.Errorf("bash waited %s for a backgrounded child", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("bash never returned after its context was cancelled")
	}
}

// The other half of the same fix, and the half WaitDelay alone does not give:
// the command runs in its own process group and the whole group is killed, so a
// cancelled command does not leave work running. Without it the shell dies and
// its children carry on doing whatever they were told to.
func TestBashKillsWhatACancelledCommandLeftRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no bash, and no group-wide kill")
	}
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Get("bash")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tool.Run(ctx, json.RawMessage(`{"cmd":"(sleep 1; touch survived) & sleep 30"}`))
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled command never returned")
	}
	// Longer than the child's own sleep, so it has had its chance.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Error("a child of the cancelled command went on to do its work")
	}
}

// A command that succeeds and leaves something running in the background is a
// thing people ask for - `make web-dev` is exactly that shape. Bounding the wait
// is right; telling the model the command failed is not.
func TestASuccessfulCommandThatBackgroundsAChildIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no bash")
	}
	prev := bashWaitDelay
	bashWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { bashWaitDelay = prev })

	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Get("bash")
	// The child inherits the pipe, so the wait runs out even though bash exited.
	out, err := tool.Run(context.Background(), json.RawMessage(`{"cmd":"sleep 30 & echo started"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "started") {
		t.Errorf("the command's own output is missing: %q", out)
	}
	if strings.Contains(out, "exit error") {
		t.Errorf("a command that succeeded was reported as failing: %q", out)
	}
	if !strings.Contains(out, "still running") {
		t.Errorf("nothing said why the output stopped: %q", out)
	}
}

func TestReadFileInContextDedup(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("PROJECT RULES"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Before marking, read_file returns the real content.
	out, _ := run(t, r, "read_file", map[string]any{"path": "AGENTS.md"})
	if !strings.Contains(out, "PROJECT RULES") {
		t.Fatalf("expected content before marking, got %q", out)
	}

	// After marking it as already-in-context, read_file returns a stand-in note.
	r.MarkInContext([]string{path})
	out, _ = run(t, r, "read_file", map[string]any{"path": "AGENTS.md"})
	if strings.Contains(out, "PROJECT RULES") || !strings.Contains(out, "already included") {
		t.Fatalf("expected in-context note, got %q", out)
	}

	// Subsets share the marking, so subagents honor it too.
	sub := r.Subset([]string{"read_file"})
	out, _ = run(t, sub, "read_file", map[string]any{"path": "AGENTS.md"})
	if strings.Contains(out, "PROJECT RULES") {
		t.Fatalf("subset should also dedup, got %q", out)
	}
}

func TestDidYouMeanSuggestions(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the real near-misses: a dir and a file off by one path segment.
	mustWrite(t, dir, "meshpump/backend/internal/nats2grpcserver/server.go", "package server")
	mustWrite(t, dir, "ragworker/crates/ragworker-grpc/src/connection.rs", "fn main() {}")

	// Missing directory -> suggest the real directory.
	_, err = run(t, r, "list_dir", map[string]any{"path": "meshpump/backend/internal/nats2grpc"})
	if err == nil || !strings.Contains(err.Error(), "did you mean") ||
		!strings.Contains(err.Error(), "nats2grpcserver") {
		t.Fatalf("expected dir suggestion, got %v", err)
	}

	// Missing file (wrong segment) -> suggest the real file.
	_, err = run(t, r, "read_file", map[string]any{"path": "ragworker/crates/ragworker-grpc/connection.rs"})
	if err == nil || !strings.Contains(err.Error(), "did you mean") ||
		!strings.Contains(err.Error(), "src/connection.rs") {
		t.Fatalf("expected file suggestion, got %v", err)
	}

	// list_dir suggests directories, not files: a file named like the query
	// must not be offered as a directory match.
	_, err = run(t, r, "list_dir", map[string]any{"path": "ragworker/crates/ragworker-grpc/connection"})
	if err == nil || strings.Contains(err.Error(), "connection.rs") {
		t.Fatalf("list_dir should not suggest a file, got %v", err)
	}
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActionableErrors(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tool, path, want string
	}{
		{"read_file", "missing.go", "list_dir or fuzzy_find"},
		{"read_file", "sub", "use list_dir"},
		{"read_file", "../escape", "working directory"},
	}
	for _, c := range cases {
		_, err := run(t, r, c.tool, map[string]any{"path": c.path})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %q: want error containing %q, got %v", c.tool, c.path, c.want, err)
		}
	}
}

func TestReadFileNumbersLines(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, r, "read_file", map[string]any{"path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "    1| alpha") || !strings.Contains(out, "    2| beta") {
		t.Fatalf("expected numbered lines, got %q", out)
	}
}

// A model that copies old_string straight from read_file output keeps the
// line-number gutter; edit_file must strip it, match, and write clean content.
func TestEditFileToleratesLineGutter(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// old/new still carry the "NNN\t" gutter the model saw in read_file output.
	_, err = run(t, r, "edit_file", map[string]any{
		"path":       "f.go",
		"old_string": "    3| func main() {}",
		"new_string": "    3| func main() { println(\"hi\") }",
	})
	if err != nil {
		t.Fatalf("gutter-tolerant edit failed: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	want := "package main\n\nfunc main() { println(\"hi\") }\n"
	if string(got) != want {
		t.Fatalf("file = %q, want %q (gutter must not leak into the file)", got, want)
	}
}

// Exact (unnumbered) old_string must still match - the gutter path is only a
// fallback, so pre-existing verbatim edits are unaffected.
func TestEditFileExactStillWorks(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("a := 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, r, "edit_file", map[string]any{
		"path": "f.go", "old_string": "a := 1", "new_string": "a := 2",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if string(got) != "a := 2\n" {
		t.Fatalf("file = %q", got)
	}
}

// The file-change hook is what puts a bot's diffs in front of the operator, and
// the wiring in cmd/aigem depends on two properties of it that nothing else
// states. cmd/aigem has no tests, so they are pinned at the layer that owns
// them.
func TestOnFileChangeReachesSubsetsAndSubagents(t *testing.T) {
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	reg.OnFileChange(func(c FileChange) { seen = append(seen, c.Path) })

	// A subset takes the hook with it: the agent runs on the subset, and a
	// delegated subagent runs on a subset of that.
	sub := reg.Subset([]string{"write_file"})
	sub.reportFileChange(FileChange{Path: "internal/auth/flow.go"})
	sub.Subset([]string{"write_file"}).reportFileChange(FileChange{Path: "internal/auth/store.go"})

	if len(seen) != 2 {
		t.Fatalf("the hook fired %d times, want once per subset level: %v", len(seen), seen)
	}
}

// Registration order is what botrun's comment turns on. A hook installed after
// a subset was taken does not reach that subset, so the wiring registers first.
func TestASubsetTakesTheHookItWasBuiltWith(t *testing.T) {
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := reg.Subset([]string{"write_file"})
	fired := false
	reg.OnFileChange(func(FileChange) { fired = true })

	sub.reportFileChange(FileChange{Path: "internal/auth/flow.go"})
	if fired {
		t.Fatal("a subset picked up a hook registered after it was taken; " +
			"if this is now true, botrun's ordering comment is wrong")
	}
}

func TestRegistryConcurrentStructuralAccess(t *testing.T) {
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("read_file is not registered")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("dynamic-%d", i)
			reg.Register(tool)
			reg.Register(&namedTool{name: name})
			reg.Get(name)
			reg.Definitions()
			reg.Names()
			reg.Unregister(name)
		}(i)
	}
	close(start)
	wg.Wait()
}

func TestRegistryConcurrentContextAndFileChange(t *testing.T) {
	reg, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(reg.Root(), "context.txt")
	if err := os.WriteFile(path, []byte("context"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	reg.OnFileChange(func(FileChange) { calls.Add(1) })
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			reg.MarkInContext([]string{path})
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		readTool, ok := reg.Get("read_file")
		if !ok {
			return
		}
		for i := 0; i < 100; i++ {
			_, _ = readTool.Run(context.Background(), json.RawMessage(`{"path":"context.txt"}`))
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			reg.reportFileChange(FileChange{Path: path})
		}
	}()
	close(start)
	wg.Wait()
	if calls.Load() != 100 {
		t.Fatalf("callback calls = %d, want 100", calls.Load())
	}
}

type namedTool struct{ name string }

func (t *namedTool) Name() string                                         { return t.name }
func (t *namedTool) Description() string                                  { return "test tool" }
func (t *namedTool) Schema() json.RawMessage                              { return json.RawMessage(`{"type":"object"}`) }
func (t *namedTool) NeedsConfirm() bool                                   { return false }
func (t *namedTool) Run(context.Context, json.RawMessage) (string, error) { return "", nil }

func TestRelToShortensAgainstTheRootAndLeavesEscapesAlone(t *testing.T) {
	root := "/home/dev/project"
	for _, c := range []struct{ name, root, path, want string }{
		{"inside", root, root + "/internal/auth/flow.go", "internal/auth/flow.go"},
		{"the root itself", root, root, "."},
		// A file outside the root stays absolute. A trail of ".." reads as an
		// escape when it is really a path the caller was granted by name.
		{"outside", root, "/etc/hosts", "/etc/hosts"},
		{"a sibling", root, "/home/dev/other/main.go", "/home/dev/other/main.go"},
		// The escape is the ".." component, not the prefix.
		{"a directory whose name starts with dots", root, root + "/..cache/x.go", "..cache/x.go"},
		{"no root", "", "/etc/hosts", "/etc/hosts"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RelTo(c.root, c.path); got != c.want {
				t.Fatalf("RelTo(%q, %q) = %q, want %q", c.root, c.path, got, c.want)
			}
		})
	}
}
