package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---- origins ----

// The bug this whole section exists for: a daemon bound to 0.0.0.0 put its own
// bind address in the allowlist, so a phone reaching it through a proxy under a
// real name got a 403 that reads as a broken server rather than as a missing
// flag. Now it refuses to start and says which flag.
func TestANonLoopbackBindNeedsAnOrigin(t *testing.T) {
	_, err := New(Config{Addr: "0.0.0.0:0", Mount: func(*http.ServeMux, func(http.HandlerFunc) http.HandlerFunc) {}})
	if err == nil {
		t.Fatal("serving on a wildcard address without an origin was accepted")
	}
	if !strings.Contains(err.Error(), "--origin") {
		t.Errorf("the refusal does not name the flag that fixes it: %v", err)
	}
}

func TestLoopbackStillNeedsNothing(t *testing.T) {
	srv := testServer(t)
	if len(srv.allowed.hosts) == 0 {
		t.Fatal("a loopback daemon allows no host at all")
	}
}

func TestConfiguredOriginsReplaceTheDerivedOnes(t *testing.T) {
	srv, err := New(Config{
		Addr:    "127.0.0.1:0",
		Origins: []string{"https://aigem.example.ts.net"},
		Mount:   func(*http.ServeMux, func(http.HandlerFunc) http.HandlerFunc) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	if !slices.Contains(srv.allowed.origins, "https://aigem.example.ts.net") {
		t.Errorf("the configured origin is not allowed: %v", srv.allowed.origins)
	}
	if !slices.Contains(srv.allowed.hosts, "aigem.example.ts.net") {
		t.Errorf("the configured host is not allowed: %v", srv.allowed.hosts)
	}
	// Loopback survives, so curl and `aigem chat` keep working against a
	// proxied daemon and the operator can still open it on the machine itself.
	_, port, _ := net.SplitHostPort(srv.Addr().String())
	if !slices.Contains(srv.allowed.hosts, "127.0.0.1:"+port) {
		t.Errorf("loopback was dropped from the hosts: %v", srv.allowed.hosts)
	}
	// And the printed URL is the one that works from outside.
	if !strings.HasPrefix(srv.URL(), "https://aigem.example.ts.net/?token=") {
		t.Errorf("the daemon prints %q, want the public origin", srv.URL())
	}
}

func TestOriginsAreCheckedWhole(t *testing.T) {
	srv, err := New(Config{
		Addr:    "127.0.0.1:0",
		Origins: []string{"https://aigem.example.ts.net"},
		Mount:   func(*http.ServeMux, func(http.HandlerFunc) http.HandlerFunc) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	for _, c := range []struct {
		origin string
		host   string
		ok     bool
	}{
		{"https://aigem.example.ts.net", "aigem.example.ts.net", true},
		// The port a browser omits and the port it does not.
		{"https://aigem.example.ts.net:443", "aigem.example.ts.net", true},
		// The scheme is half the value of the header. A page served over plain
		// HTTP from the same name is not the deployment that was configured.
		{"http://aigem.example.ts.net", "aigem.example.ts.net", false},
		{"https://aigem.example.ts.net.evil.test", "aigem.example.ts.net", false},
		{"https://evil.test", "aigem.example.ts.net", false},
		// A page cannot lie about its Origin, but it can send a Host header,
		// which is the rebinding half of the check.
		{"https://aigem.example.ts.net", "evil.test", false},
	} {
		req := &http.Request{Host: c.host, Header: http.Header{}, URL: mustURL(t, "/")}
		req.Header.Set("Origin", c.origin)
		if got := srv.originOK(req); got != c.ok {
			t.Errorf("Origin %q Host %q: allowed=%v, want %v", c.origin, c.host, got, c.ok)
		}
	}
}

func TestBadOriginsAreRefusedAtStartup(t *testing.T) {
	for _, origin := range []string{
		"aigem.example.ts.net",            // no scheme
		"ftp://aigem.example.ts.net",      // not a browser scheme
		"https://",                        // no host
		"https://aigem.example.ts.net/ui", // a URL, not an origin
		"https://user:pw@aigem.example.ts.net",
	} {
		_, err := New(Config{
			Addr:    "127.0.0.1:0",
			Origins: []string{origin},
			Mount:   func(*http.ServeMux, func(http.HandlerFunc) http.HandlerFunc) {},
		})
		if err == nil {
			t.Errorf("origin %q was accepted", origin)
		}
	}
}

// ---- the cookie ----

func TestTheTokenBuysACookieAndTheCookieWorksAlone(t *testing.T) {
	srv := testServer(t)
	base := "http://" + srv.Addr().String()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+srv.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("the exchange answered %d", res.StatusCode)
	}
	var c *http.Cookie
	for _, got := range res.Cookies() {
		if got.Name == cookieName {
			c = got
		}
	}
	if c == nil {
		t.Fatal("the exchange set no cookie")
	}
	if !c.HttpOnly {
		t.Error("the cookie is readable by script")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("the cookie is SameSite=%v, want Strict - that is half the CSRF defence", c.SameSite)
	}
	// Plain HTTP on loopback: Secure here would be a cookie the browser accepts
	// and never sends back, which is a sign-in that fails with nothing to see.
	if c.Secure {
		t.Error("the cookie is Secure on a plain-HTTP daemon")
	}

	// And it authenticates on its own, with no token anywhere.
	req, _ = http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
	req.AddCookie(c)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a request carrying only the cookie answered %d", res.StatusCode)
	}
}

func TestLogoutRevokesTheCookieEverywhere(t *testing.T) {
	srv := testServer(t)
	base := "http://" + srv.Addr().String()
	c := exchange(t, srv)

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/auth/session", nil)
	req.AddCookie(c)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// Not just cleared in the browser: the daemon must stop honouring it, or a
	// cookie captured before the logout is still a live credential.
	req, _ = http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
	req.AddCookie(c)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked cookie still answered %d", res.StatusCode)
	}
}

func TestARestartRevokesEveryCookie(t *testing.T) {
	srv := testServer(t)
	c := exchange(t, srv)
	if !srv.cookieOK(withCookie(t, c)) {
		t.Fatal("the cookie does not work before the restart")
	}
	_ = srv.Close()
	if srv.cookieOK(withCookie(t, c)) {
		t.Error("a cookie outlived the daemon that issued it")
	}
}

// The page runs the exchange on every load. Minting a session each time walked
// the table cap, so sixty-four reloads on a laptop signed the phone out.
func TestExchangingWithALiveCookieKeepsIt(t *testing.T) {
	srv := testServer(t)
	c := exchange(t, srv)

	req, _ := http.NewRequest(http.MethodPost, "http://"+srv.Addr().String()+"/api/auth/session", nil)
	req.AddCookie(c)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, got := range res.Cookies() {
		if got.Name == cookieName && got.Value != c.Value {
			t.Fatalf("the exchange replaced a live cookie with %q", got.Value)
		}
	}
	srv.mu.Lock()
	held := len(srv.cookies)
	srv.mu.Unlock()
	if held != 1 {
		t.Errorf("the daemon holds %d sessions after two exchanges from one browser, want 1", held)
	}
}

func TestTheCookieTableIsBounded(t *testing.T) {
	srv := testServer(t)
	for range maxCookieSessions + 10 {
		if _, err := srv.issueCookie(); err != nil {
			t.Fatal(err)
		}
	}
	srv.mu.Lock()
	held := len(srv.cookies)
	srv.mu.Unlock()
	if held > maxCookieSessions {
		t.Errorf("the daemon holds %d browser sessions, cap is %d", held, maxCookieSessions)
	}
}

func TestAnExpiredCookieIsNotACredential(t *testing.T) {
	srv := testServer(t)
	id, err := srv.issueCookie()
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.cookies[id] = time.Now().Add(-time.Second)
	srv.mu.Unlock()
	if srv.cookieOK(withCookie(t, &http.Cookie{Name: cookieName, Value: id})) {
		t.Error("an expired cookie was accepted")
	}
}

// Behind a proxy the hop to us is plain HTTP, so r.TLS is nil and the only
// thing that knows TLS was involved is the configured origin.
func TestTheCookieIsSecureBehindAnHTTPSOrigin(t *testing.T) {
	srv, err := New(Config{
		Addr:    "127.0.0.1:0",
		Origins: []string{"https://aigem.example.ts.net"},
		Mount:   func(*http.ServeMux, func(http.HandlerFunc) http.HandlerFunc) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	req := &http.Request{Host: "aigem.example.ts.net", Header: http.Header{}, URL: mustURL(t, "/")}
	req.Header.Set("Origin", "https://aigem.example.ts.net")
	if !srv.secureFor(req) {
		t.Error("a cookie issued to an https page is not marked Secure")
	}
	// The same daemon reached on loopback is plain HTTP, and must not be.
	plain := &http.Request{Host: srv.Addr().String(), Header: http.Header{}, URL: mustURL(t, "/")}
	if srv.secureFor(plain) {
		t.Error("a cookie issued over plain loopback HTTP is marked Secure")
	}
}

// ---- the rate limiter ----

func TestGuessingTheTokenBuysAWait(t *testing.T) {
	srv := testServer(t)
	base := "http://" + srv.Addr().String()
	guess := func() int {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for i := range authFailureBurst {
		if got := guess(); got != http.StatusUnauthorized {
			t.Fatalf("guess %d answered %d, want 401", i, got)
		}
	}
	if got := guess(); got != http.StatusTooManyRequests {
		t.Fatalf("the %dth guess answered %d, want 429", authFailureBurst+1, got)
	}
}

// The outage this ordering exists to prevent, and the reason the limiter runs
// after the credential rather than before it.
//
// Restarting the fleet revokes every cookie. An open tab's socket then fails,
// retries every 5s against a bucket that refills every 6s, and pins it at zero
// - so with the checks the other way round the operator's own browser, CLI and
// every new tab were answered 429 for as long as the old tab kept trying. On a
// proxied daemon every client shares one address, so a stranger could hold that
// state open forever for ten requests a minute.
func TestAValidCredentialIsNeverRefused(t *testing.T) {
	srv := testServer(t)
	base := "http://" + srv.Addr().String()
	ask := func(auth string) int {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+auth)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// Spend the whole burst, and then some.
	for range authFailureBurst + 5 {
		ask("wrong")
	}
	if got := ask("wrong"); got != http.StatusTooManyRequests {
		t.Fatalf("a guess from a blocked address answered %d, want 429", got)
	}
	if got := ask(srv.token); got != http.StatusOK {
		t.Fatalf("the right token from a blocked address answered %d, want 200", got)
	}
	// And that success forgave the address, so the next guess starts over.
	if got := ask("wrong"); got != http.StatusUnauthorized {
		t.Errorf("after a success the address answered %d, want a fresh 401", got)
	}
}

// Being told to wait must not itself cost a token, or the block never lifts.
func TestBeingBlockedDoesNotExtendTheBlock(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }
	for range authFailureBurst {
		l.fail("10.0.0.1")
	}
	first, blocked := l.blocked("10.0.0.1")
	if !blocked {
		t.Fatal("a full burst did not block")
	}
	for range 100 {
		l.blocked("10.0.0.1")
	}
	again, _ := l.blocked("10.0.0.1")
	if again > first {
		t.Errorf("asking whether it is blocked pushed the wait out from %s to %s", first, again)
	}
}

// A limiter that locked the operator out over their own reload would be
// defending the daemon against its user. One good credential clears the debt.
func TestASuccessfulRequestClearsTheDebt(t *testing.T) {
	l := newLimiter()
	for range authFailureBurst {
		l.fail("10.0.0.1")
	}
	if _, blocked := l.blocked("10.0.0.1"); !blocked {
		t.Fatal("the address is not blocked after a full burst")
	}
	l.clear("10.0.0.1")
	if _, blocked := l.blocked("10.0.0.1"); blocked {
		t.Error("a successful request left the address blocked")
	}
}

func TestTheDebtIsPaidOffOverTime(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }
	for range authFailureBurst {
		l.fail("10.0.0.1")
	}
	if _, blocked := l.blocked("10.0.0.1"); !blocked {
		t.Fatal("a full burst did not block")
	}
	// One token back is all it takes to try again.
	now = now.Add(authFailureWindow / authFailureBurst)
	if wait, blocked := l.blocked("10.0.0.1"); blocked {
		t.Errorf("still blocked after one token came back (wait %s)", wait)
	}
}

func TestTheFailureTableIsSwept(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }
	for i := range maxTrackedAddresses + 50 {
		l.fail(net.IPv4(10, 0, byte(i/256), byte(i%256)).String())
		// Every address pays its one failure off before the next arrives, so the
		// sweep has something to collect. A table that only ever grew would be a
		// memory leak an unauthenticated caller controls.
		now = now.Add(authFailureWindow)
	}
	l.mu.Lock()
	held := len(l.spent)
	l.mu.Unlock()
	if held > maxTrackedAddresses {
		t.Errorf("the limiter tracks %d addresses, cap is %d", held, maxTrackedAddresses)
	}
}

// The case the test above cannot reach: a flood from many addresses at once,
// where nothing has paid anything off and there is nothing to collect. A sweep
// that only drops idle entries is not a cap, and the caller chooses the size.
func TestTheFailureTableIsCappedUnderAFlood(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }
	// No clock movement at all, so every entry stays mid-burst.
	for i := range maxTrackedAddresses * 3 {
		l.fail(net.IPv4(10, byte(i/65536), byte(i/256), byte(i%256)).String())
	}
	l.mu.Lock()
	held := len(l.spent)
	l.mu.Unlock()
	if held > maxTrackedAddresses {
		t.Fatalf("the limiter tracks %d addresses under a flood, cap is %d", held, maxTrackedAddresses)
	}
	// And it still limits: the address that just failed is still counted.
	last := net.IPv4(10, byte((maxTrackedAddresses*3-1)/65536), byte((maxTrackedAddresses*3-1)/256),
		byte((maxTrackedAddresses*3-1)%256)).String()
	l.mu.Lock()
	_, tracked := l.spent[last]
	l.mu.Unlock()
	if !tracked {
		t.Error("the newest failure was evicted, so a flood is a way to never be limited")
	}
}

// ---- the socket cap ----

func TestOnlySoManySocketsAtOnce(t *testing.T) {
	var s sockets
	for i := range maxSockets {
		if !s.acquire() {
			t.Fatalf("socket %d was refused below the cap", i)
		}
	}
	if s.acquire() {
		t.Fatal("the cap does not hold")
	}
	s.release()
	if !s.acquire() {
		t.Error("a released slot was not reused")
	}
}

func TestUpgradeIsRecognisedHowBrowsersWriteIt(t *testing.T) {
	for _, c := range []struct {
		conn, upgrade string
		want          bool
	}{
		{"Upgrade", "websocket", true},
		// Firefox sends "keep-alive, Upgrade"; the tokens are case-insensitive.
		{"keep-alive, Upgrade", "WebSocket", true},
		{"close", "websocket", false},
		{"Upgrade", "h2c", false},
		{"", "", false},
	} {
		r := &http.Request{Header: http.Header{}}
		if c.conn != "" {
			r.Header.Set("Connection", c.conn)
		}
		if c.upgrade != "" {
			r.Header.Set("Upgrade", c.upgrade)
		}
		if got := isUpgrade(r); got != c.want {
			t.Errorf("Connection %q Upgrade %q: %v, want %v", c.conn, c.upgrade, got, c.want)
		}
	}
}

// ---- helpers ----

func exchange(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+srv.Addr().String()+"/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+srv.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("the exchange set no cookie")
	return nil
}

func mustURL(t *testing.T, path string) *url.URL {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func withCookie(t *testing.T, c *http.Cookie) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	return r
}

// The cap has to be held by Guard, not merely by the counter. Deleting the
// acquire/release from the wrapper left every unit test green while the daemon
// accepted unlimited sockets.
//
// It is driven with real handshake headers against a mounted handler that
// blocks, which is what a websocket handler does - both of this daemon's block
// for the life of the connection.
func TestTheSocketCapIsHeldByTheGuard(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	hold := make(chan struct{})
	inside := make(chan struct{}, maxSockets+1)
	srv, err := New(Config{
		Mount: func(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
			mux.HandleFunc("GET /api/fake/socket", guard(func(w http.ResponseWriter, _ *http.Request) {
				inside <- struct{}{}
				<-hold
			}))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	defer func() { close(hold); _ = srv.Close() }()

	dial := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr().String()+"/api/fake/socket", nil)
		req.Header.Set("Authorization", "Bearer "+srv.token)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		return http.DefaultClient.Do(req)
	}

	for i := range maxSockets {
		go func() {
			if res, derr := dial(); derr == nil {
				_ = res.Body.Close()
			}
		}()
		select {
		case <-inside:
		case <-time.After(5 * time.Second):
			t.Fatalf("handshake %d never reached the handler", i)
		}
	}

	// On a deadline: a daemon that has lost the cap accepts this one and blocks
	// it in the handler forever, and a test that hung would report the defect as
	// a timeout ten minutes later instead of as a failure here.
	past := make(chan *http.Response, 1)
	go func() {
		if res, derr := dial(); derr == nil {
			past <- res
		}
	}()
	var res *http.Response
	select {
	case res = <-past:
	case <-time.After(5 * time.Second):
		t.Fatal("the socket past the cap was accepted rather than refused")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the socket past the cap answered %d, want 503", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("the refusal does not say when to come back")
	}

	// An ordinary request is not a socket and must not be counted against them.
	plain, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr().String()+"/api/modes", nil)
	plain.Header.Set("Authorization", "Bearer "+srv.token)
	ok, err := http.DefaultClient.Do(plain)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ok.Body.Close() }()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("a plain request was refused with %d while the sockets were full", ok.StatusCode)
	}
}
