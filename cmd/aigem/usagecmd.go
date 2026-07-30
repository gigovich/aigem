package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
)

const usageUsage = `usage:
  aigem usage                  show each provider's last known quota state
  aigem usage --refresh [ref]  ask the provider for a fresh reading, then show it

Quota state is read from the headers a provider returns on ordinary calls, so
the report is as of the last request aigem (or a bot) made. --refresh sends one
small request of its own, which costs a few tokens and updates one provider -
the first authenticated one, or whichever <ref> names. The report itself always
covers every provider with a stored reading.`

func runUsageCommand(args []string) error {
	var refresh bool
	var ref string
	for _, a := range args {
		switch {
		case a == "--refresh":
			refresh = true
		case a == "-h" || a == "--help" || a == "help":
			fmt.Println(usageUsage)
			return nil
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q\n\n%s", a, usageUsage)
		default:
			if ref != "" {
				return fmt.Errorf("unexpected argument %q\n\n%s", a, usageUsage)
			}
			ref = a
		}
	}
	if ref != "" && !refresh {
		return fmt.Errorf("a model ref only applies to --refresh\n\n%s", usageUsage)
	}
	if refresh {
		// A refresh that fails - offline, rate-limited, logged out - is exactly when
		// the last stored reading is worth seeing, so the report still runs.
		if err := refreshUsage(ref); err != nil {
			fmt.Fprintln(os.Stderr, "refresh failed:", err)
		}
	}
	return showUsage()
}

// refreshUsage makes the smallest useful request against a provider so the
// response headers deliver a current reading. There is no quota endpoint to ask
// instead: the numbers only ride on real calls.
func refreshUsage(ref string) error {
	reg := defaultModelRegistry()
	if ref == "" {
		// DefaultPreferring falls back to the local model, which has no account and
		// no quota to report, so the pick is made here over authenticated providers
		// only rather than taken from it.
		def, ok := firstAuthenticatedModel(reg)
		if !ok {
			return fmt.Errorf("no authenticated provider to ask; run `aigem auth login <provider>`")
		}
		ref = def.Ref()
	}
	backend, prov, info, err := auth.OpenModel(reg, ref, defaultMaxTokens)
	if err != nil {
		return err
	}
	if !prov.NeedsAuth() {
		return fmt.Errorf("%s has no account to report quota for", info.Ref())
	}
	fmt.Fprintf(os.Stderr, "asking %s for a fresh reading...\n", info.Ref())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "Reply with the single character: ."}}
	if _, err := backend.Stream(ctx, msgs, nil, 0, func(llm.StreamEvent) {}); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	rep, ok := llm.UsageOf(backend)
	if !ok {
		return fmt.Errorf("%s does not report usage", info.Provider)
	}
	r := rep.UsageReport()
	if r.Limits.IsZero() {
		fmt.Fprintf(os.Stderr, "%s returned no quota headers\n", info.Provider)
	}
	if !r.Last.IsZero() {
		fmt.Fprintf(os.Stderr, "that request cost %d in / %d out tokens\n", r.Last.InputTokens, r.Last.OutputTokens)
	}
	return llm.SaveLimits(r.Limits)
}

// firstAuthenticatedModel picks the model a refresh should ask, in registry
// order, skipping providers that need no credential.
func firstAuthenticatedModel(reg *llm.Registry) (llm.ModelInfo, bool) {
	for _, p := range reg.Providers() {
		if p.NeedsAuth() && auth.IsAuthenticated(p.ID) && len(p.Models) > 0 {
			m := p.Models[0]
			m.Provider = p.ID
			return m, true
		}
	}
	return llm.ModelInfo{}, false
}

func showUsage() error {
	stored := llm.LoadLimits()
	reg := defaultModelRegistry()

	// List every provider that could report something - an authenticated one with
	// no snapshot yet is itself the answer to "why is this empty".
	names := map[string]bool{}
	for _, p := range reg.Providers() {
		if p.NeedsAuth() && auth.IsAuthenticated(p.ID) {
			names[p.ID] = true
		}
	}
	for id := range stored {
		names[id] = true
	}
	if len(names) == 0 {
		fmt.Println("no authenticated providers")
		return nil
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, name := range sorted {
		l, ok := stored[name]
		if !ok || l.IsZero() {
			fmt.Fprintf(w, "%s\tno reading yet\t(run `aigem usage --refresh`, or wait for the next call)\n", name)
			continue
		}
		// The per-model buckets below come from whichever model made that call, so
		// the header names it - otherwise "GPT-5.3-Codex-Spark 0%" is unreadable.
		what := "plan " + l.Plan
		if l.Plan == "" {
			what = "quota"
		}
		if l.Model != "" {
			what += " (via " + l.Model + ")"
		}
		fmt.Fprintf(w, "%s\t%s\tas of %s\n", name, what, humanAge(l.ObservedAt, now))
		for _, win := range l.Windows {
			if reset := windowReset(win, now); reset != "" {
				fmt.Fprintf(w, "  %s\t%s\t%s\n", win.Name, windowUsed(win), reset)
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\n", win.Name, windowUsed(win))
		}
		if l.Credits != "" {
			fmt.Fprintf(w, "  credits\t%s\n", l.Credits)
		}
	}
	return w.Flush()
}

func windowUsed(w llm.LimitWindow) string {
	if w.Remaining != "" {
		return w.Remaining + " left"
	}
	used := fmt.Sprintf("%g%% used", math.Round(w.UsedPercent*10)/10)
	if w.WindowMinutes > 0 {
		used += " of a " + llm.FormatDuration(time.Duration(w.WindowMinutes)*time.Minute) + " window"
	}
	return used
}

func windowReset(w llm.LimitWindow, now time.Time) string {
	if w.ResetAt.IsZero() {
		return ""
	}
	d := w.ResetAt.Sub(now)
	if d <= 0 {
		return "resets now"
	}
	return "resets in " + llm.FormatDuration(d) + " (" + w.ResetAt.Local().Format("Jan 2 15:04") + ")"
}

func humanAge(observed, now time.Time) string {
	if observed.IsZero() {
		return "unknown" // a hand-edited or truncated snapshot
	}
	if d := now.Sub(observed); d >= time.Minute {
		return llm.FormatDuration(d) + " ago"
	}
	return "just now" // includes a snapshot written by a peer whose clock is ahead
}
