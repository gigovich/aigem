package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// codexHeaders is the header set a live ChatGPT subscription response carried on
// 2026-07-27, verbatim. It is the contract this parser is written against.
func codexHeaders() http.Header {
	h := http.Header{}
	for _, kv := range [][2]string{
		{"X-Codex-Active-Limit", "premium"},
		{"X-Codex-Plan-Type", "prolite"},
		{"X-Codex-Primary-Used-Percent", "1"},
		{"X-Codex-Primary-Window-Minutes", "10080"},
		{"X-Codex-Primary-Reset-After-Seconds", "510810"},
		{"X-Codex-Primary-Reset-At", "1785646828"},
		{"X-Codex-Primary-Over-Secondary-Limit-Percent", "0"},
		{"X-Codex-Secondary-Used-Percent", "0"},
		{"X-Codex-Secondary-Window-Minutes", "0"},
		{"X-Codex-Secondary-Reset-After-Seconds", "0"},
		{"X-Codex-Secondary-Reset-At", ""},
		{"X-Codex-Bengalfox-Limit-Name", "GPT-5.3-Codex-Spark"},
		{"X-Codex-Bengalfox-Primary-Used-Percent", "12"},
		{"X-Codex-Bengalfox-Primary-Window-Minutes", "10080"},
		{"X-Codex-Bengalfox-Primary-Reset-After-Seconds", "604800"},
		{"X-Codex-Bengalfox-Secondary-Used-Percent", "0"},
		{"X-Codex-Bengalfox-Secondary-Window-Minutes", "0"},
		{"X-Codex-Credits-Balance", "0"},
		{"X-Codex-Credits-Has-Credits", "False"},
		{"X-Codex-Credits-Unlimited", "False"},
	} {
		h.Set(kv[0], kv[1])
	}
	return h
}

func TestParseLimitsCodex(t *testing.T) {
	now := time.Unix(1785136019, 0)
	l := ParseLimits(codexHeaders(), "openai", "gpt-5.6-sol", now)

	if l.Plan != "prolite" {
		t.Fatalf("plan = %q", l.Plan)
	}
	if l.Credits != "0 (none available)" {
		t.Fatalf("credits = %q", l.Credits)
	}
	// The account window and the per-model bucket are reported; the all-zero
	// secondary windows are dropped as noise.
	if len(l.Windows) != 2 {
		t.Fatalf("windows = %+v, want the primary and the named bucket", l.Windows)
	}
	primary := l.Windows[0]
	if primary.Name != "primary" || primary.UsedPercent != 1 || primary.WindowMinutes != 10080 {
		t.Fatalf("primary = %+v", primary)
	}
	// reset-at is absolute and wins over the relative reset-after-seconds.
	if got := primary.ResetAt.Unix(); got != 1785646828 {
		t.Fatalf("primary reset = %d, want the absolute reset-at", got)
	}
	bucket := l.Windows[1]
	if bucket.Name != "GPT-5.3-Codex-Spark primary" || bucket.UsedPercent != 12 {
		t.Fatalf("model bucket = %+v", bucket)
	}
	// This bucket has no reset-at, so the relative seconds are used instead.
	if want := now.Add(604800 * time.Second); !bucket.ResetAt.Equal(want) {
		t.Fatalf("bucket reset = %v, want %v", bucket.ResetAt, want)
	}
}

func TestParseLimitsIsStableAcrossCalls(t *testing.T) {
	// Header maps iterate in random order; the window order must not.
	first := ParseLimits(codexHeaders(), "openai", "m", time.Unix(1, 0))
	for i := 0; i < 50; i++ {
		got := ParseLimits(codexHeaders(), "openai", "m", time.Unix(1, 0))
		if len(got.Windows) != len(first.Windows) {
			t.Fatalf("window count changed: %d vs %d", len(got.Windows), len(first.Windows))
		}
		for j := range got.Windows {
			if got.Windows[j].Name != first.Windows[j].Name {
				t.Fatalf("window order changed between calls: %v vs %v", got.Windows, first.Windows)
			}
		}
	}
}

func TestLimitsTightest(t *testing.T) {
	l := ParseLimits(codexHeaders(), "openai", "m", time.Now())
	w, ok := l.Tightest()
	if !ok || w.Name != "GPT-5.3-Codex-Spark primary" || w.UsedPercent != 12 {
		// The per-model bucket is at 12% while the account window is at 1%: the
		// bucket is what stops work first, so it is what both the bar and the log show.
		t.Fatalf("tightest = %+v, ok=%v", w, ok)
	}

	// With no percentages at all, a remaining-count window is still worth showing.
	h := http.Header{}
	h.Set("X-Ratelimit-Remaining-Tokens", "18000")
	w, ok = ParseLimits(h, "openai", "m", time.Now()).Tightest()
	if !ok || w.Remaining == "" {
		t.Fatalf("tightest of a remaining-only provider = %+v, ok=%v", w, ok)
	}

	if _, ok := (Limits{}).Tightest(); ok {
		t.Fatal("a provider that reports nothing has no tightest window")
	}
}

func TestParseLimitsRateLimitHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Remaining-Tokens", "18000")
	h.Set("X-Ratelimit-Limit-Tokens", "30000")
	h.Set("X-Ratelimit-Reset-Tokens", "24s")
	h.Set("X-Ratelimit-Remaining-Requests", "58")

	now := time.Unix(1000, 0)
	l := ParseLimits(h, "openai", "gpt-5.4", now)
	if len(l.Windows) != 2 {
		t.Fatalf("windows = %+v", l.Windows)
	}
	if l.Windows[0].Remaining != "18000 of 30000 tokens" {
		t.Fatalf("tokens window = %+v", l.Windows[0])
	}
	if !l.Windows[0].ResetAt.Equal(now.Add(24 * time.Second)) {
		t.Fatalf("tokens reset = %v", l.Windows[0].ResetAt)
	}
	if l.Windows[1].Remaining != "58 requests" {
		t.Fatalf("requests window = %+v", l.Windows[1])
	}
}

func TestParseLimitsEmptyWhenProviderReportsNothing(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	if l := ParseLimits(h, "kimi", "kimi-for-coding", time.Now()); !l.IsZero() {
		t.Fatalf("expected zero limits, got %+v", l)
	}
}

func TestParseStreamReadsUsageChunk(t *testing.T) {
	// The usage chunk carries no choices, which is why it is read before the
	// empty-choices skip.
	sse := `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":34,` +
		`"prompt_tokens_details":{"cached_tokens":1024}}}

data: [DONE]

`
	msg, usage, err := parseStream(strings.NewReader(sse), func(StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "hi" {
		t.Fatalf("content = %q", msg.Content)
	}
	want := Usage{InputTokens: 1200, CachedTokens: 1024, OutputTokens: 34}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	if usage.Total() != 1234 {
		t.Fatalf("total = %d; cached must not be counted on top of input", usage.Total())
	}
}

func TestUsageTrackerAccumulates(t *testing.T) {
	var c Client
	c.recordUsage(t.Context(), Usage{InputTokens: 100, OutputTokens: 10})
	c.recordUsage(t.Context(), Usage{InputTokens: 200, OutputTokens: 20})
	c.recordUsage(t.Context(), Usage{}) // the provider reported no numbers for this one
	c.recordLimits(ParseLimits(codexHeaders(), "openai", "gpt-5.6-sol", time.Now()))

	r := c.UsageReport()
	if r.Calls != 2 || r.Uncounted != 1 {
		t.Fatalf("calls = %d, uncounted = %d", r.Calls, r.Uncounted)
	}
	if r.Last != (Usage{InputTokens: 200, OutputTokens: 20}) {
		t.Fatalf("last = %+v", r.Last)
	}
	if r.Total != (Usage{InputTokens: 300, OutputTokens: 30}) {
		t.Fatalf("total = %+v", r.Total)
	}
	if r.Limits.Plan != "prolite" {
		t.Fatalf("limits not retained: %+v", r.Limits)
	}
}

// Concurrent calls must each be attributed their own cost. Differencing a shared
// cumulative total from outside the lock produced negative and doubled figures.
func TestOnCallAttributesEachCallUnderConcurrency(t *testing.T) {
	var c Client
	const workers, perWorker = 8, 40

	var mu sync.Mutex
	seen := map[int]int{} // input tokens -> times reported
	c.OnCall(func(u Usage, _ UsageReport) {
		mu.Lock()
		seen[u.InputTokens]++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				c.recordUsage(t.Context(), Usage{InputTokens: w*perWorker + i + 1, OutputTokens: 1})
			}
		}(w)
	}
	wg.Wait()

	if len(seen) != workers*perWorker {
		t.Fatalf("distinct calls reported = %d, want %d", len(seen), workers*perWorker)
	}
	for tokens, times := range seen {
		if times != 1 {
			t.Fatalf("call of %d tokens reported %d times", tokens, times)
		}
		if tokens <= 0 {
			t.Fatalf("reported a non-positive cost: %d", tokens)
		}
	}
	if r := c.UsageReport(); r.Calls != workers*perWorker {
		t.Fatalf("calls = %d", r.Calls)
	}
}

func TestUsageOfUnwrapsRef(t *testing.T) {
	c := &Client{}
	c.recordUsage(t.Context(), Usage{InputTokens: 5, OutputTokens: 1})
	ref := NewRef(c)
	rep, ok := UsageOf(ref)
	if !ok {
		t.Fatal("a Ref around a counting backend must report usage")
	}
	if rep.UsageReport().Calls != 1 {
		t.Fatalf("report = %+v", rep.UsageReport())
	}
}

// A /model switch installs a new backend; an observer registered before it must
// keep firing, or a bot silently stops reporting after a switch.
func TestRefReappliesObserverOnSwitch(t *testing.T) {
	ref := NewRef(&Client{})
	var mu sync.Mutex
	var got []int
	rep, _ := UsageOf(ref)
	rep.OnCall(func(u Usage, _ UsageReport) {
		mu.Lock()
		got = append(got, u.InputTokens)
		mu.Unlock()
	})

	next := &Client{}
	ref.Set(next)
	next.recordUsage(t.Context(), Usage{InputTokens: 7, OutputTokens: 1})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("observer after switch saw %v", got)
	}
	// Totals belong to the backend that spent them, so the new one starts at zero.
	if r := ref.UsageReport(); r.Total.InputTokens != 7 {
		t.Fatalf("totals after switch = %+v", r.Total)
	}
}

type turnKey struct{}

// One client is shared by every thread a bot works on at once, so the callback
// alone cannot say which of them a call belongs to. The context is the only
// thing that travels with the call, and this is what makes it reachable.
func TestOnCallCtxCarriesTheCallContext(t *testing.T) {
	var c Client
	var billed []string
	c.OnCallCtx(func(ctx context.Context, u Usage, _ UsageReport) {
		turn, _ := ctx.Value(turnKey{}).(string)
		billed = append(billed, fmt.Sprintf("%s:%d", turn, u.InputTokens))
	})
	// The plain callback rides on the same list; adding the context must not
	// have changed what it sees.
	var plain []int
	c.OnCall(func(u Usage, r UsageReport) { plain = append(plain, r.Total.InputTokens) })

	c.recordUsage(context.WithValue(t.Context(), turnKey{}, "a"), Usage{InputTokens: 1, OutputTokens: 1})
	c.recordUsage(context.WithValue(t.Context(), turnKey{}, "b"), Usage{InputTokens: 2, OutputTokens: 1})
	c.recordUsage(t.Context(), Usage{InputTokens: 4, OutputTokens: 1}) // no turn: a heartbeat

	if want := []string{"a:1", "b:2", ":4"}; !slices.Equal(billed, want) {
		t.Fatalf("billed = %v, want %v", billed, want)
	}
	if want := []int{1, 3, 7}; !slices.Equal(plain, want) {
		t.Fatalf("OnCall saw %v, want %v", plain, want)
	}
}

// A /model switch installs a new backend; the same guarantee OnCall has.
func TestRefReappliesCtxObserverOnSwitch(t *testing.T) {
	ref := NewRef(&Client{})
	var got []string
	rep, _ := UsageOf(ref)
	rep.OnCallCtx(func(ctx context.Context, _ Usage, _ UsageReport) {
		turn, _ := ctx.Value(turnKey{}).(string)
		got = append(got, turn)
	})

	next := &Client{}
	ref.Set(next)
	next.recordUsage(context.WithValue(t.Context(), turnKey{}, "a"), Usage{InputTokens: 7})

	if want := []string{"a"}; !slices.Equal(got, want) {
		t.Fatalf("observer after switch saw %v, want %v", got, want)
	}
}

func TestStreamRecordsUsageAndLimits(t *testing.T) {
	var sawStreamOptions bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawStreamOptions = strings.Contains(string(body), `"stream_options":{"include_usage":true}`)
		for k, v := range codexHeaders() {
			w.Header().Set(k, v[0])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Info: ModelInfo{Provider: "openai", ID: "m"}})
	if _, err := c.Stream(context.Background(), []Message{{Role: RoleUser, Content: "hi"}},
		nil, 0, func(StreamEvent) {}); err != nil {
		t.Fatal(err)
	}
	if !sawStreamOptions {
		t.Fatal("the usage opt-in was not sent; every token count would silently be zero")
	}
	r := c.UsageReport()
	if r.Total != (Usage{InputTokens: 80, OutputTokens: 9}) {
		t.Fatalf("total = %+v", r.Total)
	}
	if r.Limits.Plan != "prolite" {
		t.Fatalf("quota headers not recorded: %+v", r.Limits)
	}
}

func TestStreamRecordsLimitsOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range codexHeaders() {
			w.Header().Set(k, v[0])
		}
		http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Info: ModelInfo{Provider: "openai", ID: "m"}})
	var calls, limits int
	c.OnCall(func(Usage, UsageReport) { calls++ })
	c.OnLimits(func(Limits) { limits++ })

	if _, err := c.Stream(context.Background(), []Message{{Role: RoleUser}}, nil, 0,
		func(StreamEvent) {}); err == nil {
		t.Fatal("expected the 429 to surface")
	}
	// A 429 is the reading that matters most, so it must be kept even though the
	// call produced no tokens.
	if r := c.UsageReport(); r.Limits.Plan != "prolite" {
		t.Fatalf("limits from the 429 were dropped: %+v", r.Limits)
	}
	// A rejected call never reaches the usage path, which is exactly why anything
	// that persists the reading has to hang off the quota callback instead.
	if calls != 0 || limits != 1 {
		t.Fatalf("on a 429: usage callbacks = %d (want 0), quota callbacks = %d (want 1)", calls, limits)
	}
}

// A gateway that rejects the usage opt-in must cost token counts, not every call.
func TestStreamFallsBackWhenStreamOptionsRejected(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempts++
		if strings.Contains(string(body), "stream_options") {
			http.Error(w, `{"error":{"message":"unknown argument stream_options"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Info: ModelInfo{Provider: "x", ID: "m"}})
	msg, err := c.Stream(context.Background(), []Message{{Role: RoleUser}}, nil, 0, func(StreamEvent) {})
	if err != nil || msg.Content != "ok" {
		t.Fatalf("msg = %+v, err = %v", msg, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the rejected one plus the retry", attempts)
	}
	// The rejection must latch, or every later call pays for the same 400.
	if _, err := c.Stream(context.Background(), []Message{{Role: RoleUser}}, nil, 0,
		func(StreamEvent) {}); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want one more; the opt-in was retried after a rejection", attempts)
	}
}
