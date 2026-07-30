package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultTimeout = 60 * time.Second
	// maxHookOutput caps the stdout/stderr captured from one hook so a runaway
	// command cannot exhaust memory.
	maxHookOutput = 1 << 20 // 1 MiB
)

// capWriter buffers up to max bytes and silently drops the rest, always
// reporting a full write so the hook is never blocked on a full pipe.
type capWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := maxHookOutput - w.n; room > 0 {
		if room > len(p) {
			room = len(p)
		}
		w.buf.Write(p[:room])
		w.n += room
	}
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }

// hookOutput is the JSON a hook command may print to stdout (exit 0) to steer
// the agent. Both the top-level and hookSpecificOutput fields are honored.
type hookOutput struct {
	Continue      *bool  `json:"continue"`
	StopReason    string `json:"stopReason"`
	SystemMessage string `json:"systemMessage"`
	SessionTitle  string `json:"sessionTitle"`
	Decision      string `json:"decision"` // "block" | "approve"
	Reason        string `json:"reason"`
	HookSpecific  struct {
		PermissionDecision       string          `json:"permissionDecision"` // allow|deny|ask
		PermissionDecisionReason string          `json:"permissionDecisionReason"`
		UpdatedInput             json.RawMessage `json:"updatedInput"`
		UpdatedToolOutput        *string         `json:"updatedToolOutput"`
		AdditionalContext        string          `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// RunBounded is Run with an overall deadline, used for fire-and-forget
// lifecycle events (SessionEnd, Notification) so a slow hook cannot stall quit.
func (r *Runner) RunBounded(event string, in Input, timeout time.Duration) Decision {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.Run(ctx, event, in)
}

// Run executes the settings (base) hooks for event. See RunScoped to also run
// turn-scoped (skill) hooks.
func (r *Runner) Run(ctx context.Context, event string, in Input) Decision {
	return r.RunScoped(ctx, event, in, nil)
}

// RunScoped executes every matching base hook then every matching scoped (skill)
// hook for event, in order, and returns the aggregated decision. With no
// matching hooks it returns an allowing decision.
func (r *Runner) RunScoped(ctx context.Context, event string, in Input,
	scoped map[string][]Matcher) Decision {
	hooksToRun := r.matching(event, in.ToolName, scoped)
	dec := Decision{Continue: true}
	if len(hooksToRun) == 0 {
		return dec
	}

	in.HookEventName = event
	r.mu.RLock()
	in.SessionID, in.TranscriptPath, in.Cwd = r.sessionID, r.transcriptPath, r.cwd
	dir := r.dir
	debug := r.debug
	r.mu.RUnlock()
	stdin, _ := json.Marshal(in)

	decided := false // whether allow/ask was already chosen by a prior hook
	var ctxParts []string
	for _, h := range hooksToRun {
		out, stderr, code := runOne(ctx, h, dir, stdin)
		if debug != nil {
			fmt.Fprintf(debug, "[hook] %s %q exit=%d stdout=%q stderr=%q\n",
				event, h.Command, code, firstLine(out), firstLine(stderr))
		}
		switch {
		case code == 2:
			dec.Block, dec.Allow, dec.Ask = true, false, false
			if dec.Reason == "" {
				dec.Reason = strings.TrimSpace(stderr)
			}
			continue
		case code != 0:
			if line := firstLine(stderr); line != "" {
				dec.Notices = append(dec.Notices, line)
			}
			continue
		}
		o, ok := parseOutput(out)
		if !ok {
			continue
		}
		if o.Continue != nil && !*o.Continue {
			dec.Continue = false
			if o.StopReason != "" {
				dec.StopReason = o.StopReason
			}
		}
		if o.SystemMessage != "" {
			dec.SystemMessage = joinNonEmpty(dec.SystemMessage, o.SystemMessage)
		}
		if o.SessionTitle != "" {
			dec.SessionTitle = o.SessionTitle
		}
		if c := o.HookSpecific.AdditionalContext; c != "" {
			ctxParts = append(ctxParts, c)
		}
		if o.HookSpecific.UpdatedInput != nil {
			dec.UpdatedInput = o.HookSpecific.UpdatedInput
		}
		if o.HookSpecific.UpdatedToolOutput != nil {
			dec.UpdatedOutput = o.HookSpecific.UpdatedToolOutput
		}

		switch perm := o.HookSpecific.PermissionDecision; {
		case o.Decision == "block" || perm == "deny":
			dec.Block, dec.Allow, dec.Ask = true, false, false
			if dec.Reason == "" {
				dec.Reason = firstNonEmpty(o.HookSpecific.PermissionDecisionReason, o.Reason)
			}
		case (perm == "allow" || o.Decision == "approve") && !dec.Block && !decided:
			dec.Allow, decided = true, true
		case perm == "ask" && !dec.Block && !decided:
			dec.Ask, decided = true, true
		}
	}
	dec.Context = strings.Join(ctxParts, "\n")
	if dec.Block && dec.Reason == "" {
		dec.Reason = "blocked by a " + event + " hook"
	}
	return dec
}

// runOne executes a single hook command and returns stdout, stderr, exit code.
func runOne(ctx context.Context, h Hook, dir string, stdin []byte) (string, string, int) {
	timeout := defaultTimeout
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if len(h.Args) > 0 {
		cmd = exec.CommandContext(ctx, h.Command, h.Args...)
	} else {
		shell, flag := "bash", "-c"
		if h.Shell != "" {
			shell = h.Shell
		}
		cmd = exec.CommandContext(ctx, shell, flag, h.Command)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+dir)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr capWriter
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// WaitDelay bounds how long Run blocks if a surviving child keeps the output
	// pipes open after the hook itself is gone.
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
			if stderr.n == 0 {
				stderr.Write([]byte(err.Error()))
			}
		}
	}
	return stdout.String(), stderr.String(), code
}

// parseOutput parses a hook's stdout as JSON, returning ok=false for non-JSON
// (treated as no decision).
func parseOutput(s string) (hookOutput, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return hookOutput{}, false
	}
	var o hookOutput
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return hookOutput{}, false
	}
	return o, true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}
