package llm

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Usage is what one model call cost, as the provider counted it. CachedTokens is
// the part of InputTokens the provider served from its prompt cache; it is
// already included in InputTokens, never added on top.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	CachedTokens int `json:"cached_tokens,omitempty"`
	OutputTokens int `json:"output_tokens"`
}

func (u Usage) Total() int   { return u.InputTokens + u.OutputTokens }
func (u Usage) IsZero() bool { return u == Usage{} }

func (u Usage) add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		CachedTokens: u.CachedTokens + o.CachedTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
	}
}

// LimitWindow is one quota bucket the provider reports: a percentage consumed of
// a rolling window, or a remaining count for the classic per-minute API limits.
type LimitWindow struct {
	Name          string    `json:"name"`
	UsedPercent   float64   `json:"used_percent,omitempty"`
	WindowMinutes int       `json:"window_minutes,omitempty"`
	ResetAt       time.Time `json:"reset_at,omitzero"`
	Remaining     string    `json:"remaining,omitempty"` // e.g. "18000 of 30000 tokens"
}

// Limits is a provider's quota state as of one response. Providers report wildly
// different things, so everything is optional and callers render what is set.
type Limits struct {
	Provider   string        `json:"provider,omitempty"`
	Model      string        `json:"model,omitempty"`
	Plan       string        `json:"plan,omitempty"`
	Credits    string        `json:"credits,omitempty"`
	Windows    []LimitWindow `json:"windows,omitempty"`
	ObservedAt time.Time     `json:"observed_at,omitzero"`
}

func (l Limits) IsZero() bool { return l.Plan == "" && l.Credits == "" && len(l.Windows) == 0 }

// UsageReport is everything a backend knows about what it has spent and how
// close the account is to its limits. Uncounted is how many calls the provider
// reported no numbers for, so a total can never quietly understate the spend.
//
// Calls excludes those, so three calls of which one reported nothing read here
// as "calls=2 uncounted=1". chat.Usage, which records the same calls against a
// thread, counts them all in Calls instead; the two are read side by side and
// the difference is worth knowing before comparing them.
type UsageReport struct {
	Last      Usage
	Total     Usage
	Calls     int
	Uncounted int
	Limits    Limits
}

// UsageReporter is implemented by backends that count tokens and read quota
// headers. It is optional: a backend that reports nothing simply is not one.
type UsageReporter interface {
	UsageReport() UsageReport
	// OnCall registers a callback fired once per model call, with that call's own
	// cost. Attribution has to happen here: Stream returns no usage, so a wrapper
	// could only reconstruct a per-call figure by differencing a shared total,
	// which two concurrent calls interleave into nonsense. A zero Usage means the
	// provider reported none. Callbacks must not block; they run on the calling
	// goroutine right after the response.
	OnCall(func(Usage, UsageReport))
	// OnCallCtx is OnCall with the context the call was made under. It exists
	// because one client is shared by every concurrent thread a bot is working
	// on, so the callback alone cannot say which of them to bill: the context is
	// the only thing that travels with the call. A caller that does not care
	// which turn the call belonged to uses OnCall, which is this with the
	// context dropped.
	OnCallCtx(func(context.Context, Usage, UsageReport))
	// OnLimits registers a callback fired whenever a response carries quota
	// headers, including the responses that failed. A rejected call reports no
	// usage, so OnCall never sees a 429 - and a 429 is the reading most worth
	// keeping.
	OnLimits(func(Limits))
}

// UsageOf returns b's reporter, unwrapping a Ref so callers holding the shared
// swappable handle see the live backend's numbers. Note that Retrying wraps a
// bare streamer and cannot be unwrapped: hold the Ref, not the decorated chain,
// to observe usage.
func UsageOf(b Backend) (UsageReporter, bool) {
	if r, ok := b.(*Ref); ok {
		return r, r.usable()
	}
	u, ok := b.(UsageReporter)
	return u, ok
}

// usageTracker is embedded in each backend to accumulate what its calls cost.
type usageTracker struct {
	mu        sync.Mutex
	last      Usage
	total     Usage
	calls     int
	uncounted int
	limits    Limits
	onCall    []func(context.Context, Usage, UsageReport)
	onLimits  []func(Limits)
}

// recordUsage books one call. A zero Usage still counts as a call - the provider
// simply did not say what it cost (an aborted runaway, or a backend that reports
// no numbers) - and is tracked separately so the totals stay honest.
func (t *usageTracker) recordUsage(ctx context.Context, u Usage) {
	t.mu.Lock()
	if u.IsZero() {
		t.uncounted++
	} else {
		t.last, t.total, t.calls = u, t.total.add(u), t.calls+1
	}
	rep := t.reportLocked()
	// Cloned, and fired after the unlock: a callback is free to call back into
	// UsageReport or register another one without deadlocking.
	subs := slices.Clone(t.onCall)
	t.mu.Unlock()
	for _, f := range subs {
		f(ctx, u, rep)
	}
}

func (t *usageTracker) recordLimits(l Limits) {
	if l.IsZero() {
		return
	}
	t.mu.Lock()
	t.limits = l
	subs := slices.Clone(t.onLimits)
	t.mu.Unlock()
	for _, f := range subs {
		f(l)
	}
}

// UsageReport implements UsageReporter.
func (t *usageTracker) UsageReport() UsageReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reportLocked()
}

func (t *usageTracker) reportLocked() UsageReport {
	return UsageReport{Last: t.last, Total: t.total, Calls: t.calls,
		Uncounted: t.uncounted, Limits: t.limits}
}

// OnCall implements UsageReporter.
func (t *usageTracker) OnCall(f func(Usage, UsageReport)) {
	if f == nil {
		return
	}
	t.OnCallCtx(func(_ context.Context, u Usage, r UsageReport) { f(u, r) })
}

// OnCallCtx implements UsageReporter.
func (t *usageTracker) OnCallCtx(f func(context.Context, Usage, UsageReport)) {
	if f == nil {
		return
	}
	t.mu.Lock()
	t.onCall = append(t.onCall, f)
	t.mu.Unlock()
}

// OnLimits implements UsageReporter.
func (t *usageTracker) OnLimits(f func(Limits)) {
	if f == nil {
		return
	}
	t.mu.Lock()
	t.onLimits = append(t.onLimits, f)
	t.mu.Unlock()
}

// Tightest returns the window closest to cutting the account off: the highest
// percentage used, else a remaining-count window when that is all the provider
// reports. Both the status bar and the bot log show one window, and they must
// agree on which.
func (l Limits) Tightest() (LimitWindow, bool) {
	var worst LimitWindow
	for _, w := range l.Windows {
		if w.UsedPercent > worst.UsedPercent {
			worst = w
		}
	}
	if worst.UsedPercent > 0 {
		return worst, true
	}
	for _, w := range l.Windows {
		if w.Remaining != "" {
			return w, true
		}
	}
	return LimitWindow{}, false
}

// codexWindowRe matches the used-percent header of one quota bucket. The optional
// middle group is a bucket name the account-wide headers omit and per-model ones
// carry (e.g. "bengalfox"), which is a codename that changes between releases -
// hence discovering buckets from the headers present rather than a fixed list.
var codexWindowRe = regexp.MustCompile(`^x-codex-(?:(.+)-)?(primary|secondary)-used-percent$`)

// ParseLimits reads whatever quota headers a response carries. It understands the
// ChatGPT/Codex x-codex-* family and the classic x-ratelimit-* family, and
// returns a zero Limits when the provider reports neither.
func ParseLimits(h http.Header, provider, model string, now time.Time) Limits {
	l := Limits{Provider: provider, Model: model, ObservedAt: now}
	l.Plan = h.Get("X-Codex-Plan-Type")
	l.Credits = codexCredits(h)
	l.Windows = append(codexWindows(h, now), rateLimitWindows(h, now)...)
	if l.IsZero() {
		return Limits{}
	}
	return l
}

func codexWindows(h http.Header, now time.Time) []LimitWindow {
	var out []LimitWindow
	for key := range h {
		m := codexWindowRe.FindStringSubmatch(strings.ToLower(key))
		if m == nil {
			continue
		}
		prefix, kind := m[1], m[2]
		field := func(name string) string {
			if prefix == "" {
				return h.Get("X-Codex-" + kind + "-" + name)
			}
			return h.Get("X-Codex-" + prefix + "-" + kind + "-" + name)
		}
		w := LimitWindow{
			Name:          codexWindowName(h, prefix, kind),
			UsedPercent:   parseFloat(h.Get(key)),
			WindowMinutes: parseInt(field("window-minutes")),
		}
		// reset-at is an absolute unix second; reset-after-seconds is relative and
		// is the only one some responses carry.
		if ts := parseInt(field("reset-at")); ts > 0 {
			w.ResetAt = time.Unix(int64(ts), 0)
		} else if secs := parseInt(field("reset-after-seconds")); secs > 0 {
			w.ResetAt = now.Add(time.Duration(secs) * time.Second)
		}
		// An unused bucket reports zeros across the board; reporting it as "0% of a
		// 0-minute window" would be noise, not information.
		if w.UsedPercent == 0 && w.WindowMinutes == 0 {
			continue
		}
		out = append(out, w)
	}
	sortWindows(out)
	return out
}

func codexWindowName(h http.Header, prefix, kind string) string {
	if prefix == "" {
		return kind
	}
	if name := h.Get("X-Codex-" + prefix + "-Limit-Name"); name != "" {
		return name + " " + kind
	}
	return prefix + " " + kind
}

func codexCredits(h http.Header) string {
	if strings.EqualFold(h.Get("X-Codex-Credits-Unlimited"), "true") {
		return "unlimited"
	}
	bal := h.Get("X-Codex-Credits-Balance")
	if bal == "" {
		return ""
	}
	if strings.EqualFold(h.Get("X-Codex-Credits-Has-Credits"), "false") {
		return bal + " (none available)"
	}
	return bal
}

// rateLimitWindows reads the classic per-minute headers an API key gets. They
// count down remaining units rather than up a percentage, so they land in
// Remaining instead of UsedPercent.
func rateLimitWindows(h http.Header, now time.Time) []LimitWindow {
	var out []LimitWindow
	for _, unit := range []string{"tokens", "requests"} {
		remaining := h.Get("X-Ratelimit-Remaining-" + unit)
		if remaining == "" {
			continue
		}
		w := LimitWindow{Name: unit + " per minute", Remaining: remaining + " " + unit}
		if limit := h.Get("X-Ratelimit-Limit-" + unit); limit != "" {
			w.Remaining = remaining + " of " + limit + " " + unit
		}
		if d, err := time.ParseDuration(h.Get("X-Ratelimit-Reset-" + unit)); err == nil && d > 0 {
			w.ResetAt = now.Add(d)
		}
		out = append(out, w)
	}
	return out
}

// sortWindows puts the account-wide buckets first and orders the rest by name, so
// a report does not reshuffle between calls (header map order is random).
func sortWindows(w []LimitWindow) {
	rank := func(n string) int {
		switch n {
		case "primary":
			return 0
		case "secondary":
			return 1
		}
		return 2
	}
	slices.SortFunc(w, func(a, b LimitWindow) int {
		if ra, rb := rank(a.Name), rank(b.Name); ra != rb {
			return ra - rb
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// FormatDuration renders a coarse duration for quota reporting. Quota windows
// run in days and hours, where "5d21h" reads better than "5d21h13m47s".
func FormatDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		days, hours := int(d/(24*time.Hour)), int((d%(24*time.Hour))/time.Hour)
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	case d >= time.Hour:
		hours, mins := int(d/time.Hour), int((d%time.Hour)/time.Minute)
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, mins)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}
