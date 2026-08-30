package web

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- origins ----

// The bug this whole section exists for: a daemon bound to 0.0.0.0 put its own
// bind address in the allowlist, so a phone reaching it through a proxy under a
// real name got a 403 that reads as a broken server rather than as a missing
// flag. Now it refuses to start and says which flag.
func TestANonLoopbackBindNeedsAnOrigin(t *testing.T) {
	_, err := New(Config{Addr: "0.0.0.0:0"})
	if err == nil {
		t.Fatal("serving on a wildcard address without an origin was accepted")
	}
	if !strings.Contains(err.Error(), "--origin") {
		t.Errorf("the refusal does not name the flag that fixes it: %v", err)
	}
}

// The other half of the same decision: with the name stated, the bind the rule
// exists to refuse is the one it now allows.
func TestAStatedOriginUnlocksARoutableBind(t *testing.T) {
	srv, err := New(Config{Addr: "0.0.0.0:0", Origins: []string{"https://aigem.example.ts.net"}})
	if err != nil {
		t.Fatalf("a wildcard bind with an origin was refused: %v", err)
	}
	_ = srv.Close()
}

// checkBound is the backstop for what checkBind can only approximate, so the
// same relaxation has to reach it - otherwise --origin is accepted at the string
// and refused at the socket, which is a flag that does nothing.
func TestCheckBoundFollowsTheSameRule(t *testing.T) {
	routable := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}
	if err := checkBound(routable, nil); err == nil {
		t.Error("a routable socket was accepted with no origin stated")
	}
	if err := checkBound(routable, []string{"https://aigem.example.ts.net"}); err != nil {
		t.Errorf("a routable socket was refused despite a stated origin: %v", err)
	}
}

func TestLoopbackStillNeedsNothing(t *testing.T) {
	srv := newTestServer(t, Config{})
	if len(srv.allowed.hosts) == 0 {
		t.Fatal("a loopback daemon allows no host at all")
	}
}

func TestConfiguredOriginsReplaceTheDerivedOnes(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0", Origins: []string{"https://aigem.example.ts.net"}})

	if !slices.Contains(srv.allowed.origins, "https://aigem.example.ts.net") {
		t.Errorf("the configured origin is not allowed: %v", srv.allowed.origins)
	}
	if !slices.Contains(srv.allowed.hosts, "aigem.example.ts.net") {
		t.Errorf("the configured host is not allowed: %v", srv.allowed.hosts)
	}
	// Loopback survives, so curl keeps working against a proxied daemon and the
	// operator can still open it on the machine itself.
	_, port, _ := net.SplitHostPort(srv.Addr().String())
	if !slices.Contains(srv.allowed.hosts, "127.0.0.1:"+port) {
		t.Errorf("loopback was dropped from the hosts: %v", srv.allowed.hosts)
	}
	// And the printed URL is the one that works from outside.
	if !strings.HasPrefix(srv.SignInURL(), "https://aigem.example.ts.net/?token=") {
		t.Errorf("the daemon prints %q, want the public origin", srv.SignInURL())
	}
}

func TestOriginsAreCheckedWhole(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0", Origins: []string{"https://aigem.example.ts.net"}})

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

// A browser lowercases the host it puts in an Origin header. An allowlist entry
// that kept the operator's capitals could never match one, which is a 403 that
// reads as a broken server - the exact failure --origin exists to prevent.
func TestAnOriginIsMatchedCaseInsensitively(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0", Origins: []string{"https://AIGEM.Example.TS.net"}})

	req := &http.Request{Host: "aigem.example.ts.net", Header: http.Header{}, URL: mustURL(t, "/")}
	req.Header.Set("Origin", "https://aigem.example.ts.net")
	if !srv.originOK(req) {
		t.Errorf("a browser's lowercase Origin was refused against %v", srv.allowed.origins)
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
		srv, err := New(Config{Addr: "127.0.0.1:0", Origins: []string{origin}})
		if err == nil {
			_ = srv.Close()
			t.Errorf("origin %q was accepted", origin)
		}
	}
}

// A refused origin must not leave the port bound: the operator fixes the flag
// and starts again, and a leaked listener makes the second attempt fail with a
// message about the address instead.
func TestARefusedOriginReleasesThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if _, err := New(Config{Addr: addr, Origins: []string{"nonsense"}}); err == nil {
		t.Fatal("a bad origin was accepted")
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port is still held after the refusal: %v", err)
	}
	_ = again.Close()
}

// The bind address used to stay on the allowlist even when an origin replaced
// the derived list, so a daemon on a routable address declared https-only also
// answered to http://<that address>:7777 - which is what lets a plain-HTTP page
// on that name pass the guard, and what made secureFor hand it a cookie with no
// Secure flag.
func TestAStatedOriginDropsTheBindAddress(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "0.0.0.0:0", Origins: []string{"https://aigem.example.ts.net"}})
	// Read back rather than assumed: a wildcard bind comes back as "[::]:port"
	// on a dual-stack host, and asserting on the string that was asked for would
	// be a test of nothing.
	bound := srv.Addr().String()
	_, port, err := net.SplitHostPort(bound)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(srv.allowed.hosts, bound) {
		t.Errorf("the bind address %s is still an accepted Host: %v", bound, srv.allowed.hosts)
	}
	if slices.Contains(srv.allowed.origins, "http://"+bound) {
		t.Errorf("the bind address %s is still an accepted Origin: %v", bound, srv.allowed.origins)
	}
	// The stated name is there, and so are the loopback names, which is the part
	// that deliberately survives.
	if !slices.Contains(srv.allowed.hosts, "aigem.example.ts.net") {
		t.Errorf("the stated name is not allowed: %v", srv.allowed.hosts)
	}
	if !slices.Contains(srv.allowed.hosts, "127.0.0.1:"+port) {
		t.Errorf("loopback was dropped: %v", srv.allowed.hosts)
	}
}

// Allowlisting [::1] on a daemon bound to 127.0.0.1 alone names an origin this
// daemon never answers on - and any other process on the machine is free to
// bind it and serve a page from it.
func TestOnlyTheLoopbackNamesThisSocketAnswersOnAreAllowed(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0"})
	_, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(srv.allowed.origins, "http://[::1]:"+port) {
		t.Errorf("a daemon bound to 127.0.0.1 allows an IPv6 loopback origin: %v", srv.allowed.origins)
	}
	for _, want := range []string{"http://127.0.0.1:" + port, "http://localhost:" + port} {
		if !slices.Contains(srv.allowed.origins, want) {
			t.Errorf("%s is not allowed: %v", want, srv.allowed.origins)
		}
	}
}

// A hostname is case-insensitive to the resolver and may carry a trailing root
// dot. Refusing either is a 403 that reads as a broken server, which is the
// failure the allowlist exists to avoid - and isLoopbackHost was already made
// case-insensitive for exactly this reason.
func TestAHostIsMatchedTheWayAResolverWouldRead(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0", Origins: []string{"https://aigem.example.ts.net"}})
	for _, host := range []string{
		"aigem.example.ts.net",
		"AIGEM.Example.TS.net",
		"aigem.example.ts.net.",
	} {
		req := &http.Request{Host: host, Header: http.Header{}, URL: mustURL(t, "/")}
		if !srv.originOK(req) {
			t.Errorf("Host %q was refused against %v", host, srv.allowed.hosts)
		}
	}
	// Still nothing looser than an exact name.
	for _, host := range []string{"aigem.example.ts.net.evil.test", "evil.test", "aigem.example.ts"} {
		req := &http.Request{Host: host, Header: http.Header{}, URL: mustURL(t, "/")}
		if srv.originOK(req) {
			t.Errorf("Host %q was accepted", host)
		}
	}
}

// A browser sends an internationalised name in punycode, so an allowlist entry
// holding the unicode spelling matches nothing, ever. Refusing it at startup is
// one lookup for the operator; accepting it is a daemon that 403s every request
// with no way to tell why.
func TestAUnicodeOriginIsRefusedWithTheFix(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", Origins: []string{"https://例え.jp"}})
	if err == nil {
		_ = srv.Close()
		t.Fatal("a unicode origin was accepted")
	}
	if !strings.Contains(err.Error(), "punycode") {
		t.Errorf("error = %v, want it to name the form that works", err)
	}
	// And the punycode form is accepted.
	ok, err := New(Config{Addr: "127.0.0.1:0", Origins: []string{"https://xn--r8jz45g.jp"}})
	if err != nil {
		t.Fatalf("the punycode form was refused: %v", err)
	}
	_ = ok.Close()
}

// ---- the token in the URL ----

// The whole sign-in story: the daemon prints .../?token=..., and the page spends
// it on its first request. A browser cannot set a header on a websocket
// handshake either, so the query form is the only credential that route will
// ever have.
func TestTheTokenInTheQueryStringAuthenticates(t *testing.T) {
	srv := newTestServer(t, Config{})

	res := exchangeWith(t, srv, func(r *http.Request) {
		r.URL.RawQuery = "token=" + srv.token
	})
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("the token from the query string answered %d, want 204", res.StatusCode)
	}
	var got *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			got = c
		}
	}
	if got == nil {
		t.Error("the exchange set no cookie for a query-string token")
	}

	// And the printed URL is that request, spelled out.
	if !strings.Contains(srv.SignInURL(), "?token="+srv.token) {
		t.Errorf("SignInURL = %q, which is not what the query check reads", srv.SignInURL())
	}
	wrong := exchangeWith(t, srv, func(r *http.Request) { r.URL.RawQuery = "token=nonsense" })
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong token in the query answered %d, want 401", wrong.StatusCode)
	}
}

// An Authorization header that is not a bearer token is a credential this
// daemon does not accept, and it must not fall through to the query string -
// which would let a page smuggle one past a proxy that strips Authorization.
func TestANonBearerAuthorizationDoesNotFallBackToTheQuery(t *testing.T) {
	srv := newTestServer(t, Config{})
	res := exchangeWith(t, srv, func(r *http.Request) {
		r.Header.Set("Authorization", "Basic "+srv.token)
		r.URL.RawQuery = "token=" + srv.token
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a Basic credential answered %d, want 401", res.StatusCode)
	}
}

// ---- the cookie ----

func TestTheTokenBuysACookieAndTheCookieWorksAlone(t *testing.T) {
	srv := newTestServer(t, Config{})

	res := exchangeWith(t, srv, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+srv.token)
	})
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
	alone := exchangeWith(t, srv, func(r *http.Request) { r.AddCookie(c) })
	if alone.StatusCode != http.StatusNoContent {
		t.Fatalf("a request carrying only the cookie answered %d", alone.StatusCode)
	}
}

func TestLogoutRevokesTheCookieEverywhere(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := exchange(t, srv)

	req, err := http.NewRequest(http.MethodDelete, srv.Base()+"api/auth/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(c)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()

	// Not just cleared in the browser: the daemon must stop honouring it, or a
	// cookie captured before the logout is still a live credential.
	after := exchangeWith(t, srv, func(r *http.Request) { r.AddCookie(c) })
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked cookie still answered %d", after.StatusCode)
	}
}

func TestARestartRevokesEveryCookie(t *testing.T) {
	srv := newTestServer(t, Config{})
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
	srv := newTestServer(t, Config{})
	c := exchange(t, srv)

	res := exchangeWith(t, srv, func(r *http.Request) { r.AddCookie(c) })
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
	srv := newTestServer(t, Config{})
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
	srv := newTestServer(t, Config{})
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

// The loopback origins are on the allowlist by construction, so a request can
// carry the https name in Host and a plain-HTTP loopback Origin and still pass
// the guard. Reading the header first then handed an https deployment a cookie
// with no Secure flag, sent in cleartext ever after. A proxy that rewrites
// Origin to the backend's own is not hypothetical - this repository ships one
// for the dev server.
func TestTheStatedOriginDecidesSecureNotTheHeader(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0", Origins: []string{"https://aigem.example.ts.net"}})
	_, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{Host: "aigem.example.ts.net", Header: http.Header{}, URL: mustURL(t, "/")}
	req.Header.Set("Origin", "http://127.0.0.1:"+port)
	if !srv.originOK(req) {
		t.Fatal("the request this test is about does not pass the guard; it proves nothing")
	}
	if !srv.secureFor(req) {
		t.Error("a request that arrived under the https name got a cookie with no Secure flag")
	}
}

// Behind a proxy the hop to us is plain HTTP, so r.TLS is nil and the only
// thing that knows TLS was involved is the configured origin.
func TestTheCookieIsSecureBehindAnHTTPSOrigin(t *testing.T) {
	srv := newTestServer(t, Config{Addr: "127.0.0.1:0", Origins: []string{"https://aigem.example.ts.net"}})

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
	srv := newTestServer(t, Config{})
	guess := func() int {
		res := exchangeWith(t, srv, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
		})
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
// Restarting the daemon revokes every cookie it did not write to a file. An
// open tab's socket then fails, retries every 5s against a bucket that refills
// every 6s, and pins it at zero - so with the checks the other way round the
// operator's own browser, CLI and every new tab were answered 429 for as long
// as the old tab kept trying. On a proxied daemon every client shares one
// address, so a stranger could hold that state open forever for ten requests a
// minute.
func TestAValidCredentialIsNeverRefused(t *testing.T) {
	srv := newTestServer(t, Config{})
	ask := func(auth string) int {
		res := exchangeWith(t, srv, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+auth)
		})
		return res.StatusCode
	}
	// Spend the whole burst, and then some.
	for range authFailureBurst + 5 {
		ask("wrong")
	}
	if got := ask("wrong"); got != http.StatusTooManyRequests {
		t.Fatalf("a guess from a blocked address answered %d, want 429", got)
	}
	if got := ask(srv.token); got != http.StatusNoContent {
		t.Fatalf("the right token from a blocked address answered %d, want 204", got)
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

// The limiter counts by peer address and nothing else. Honouring
// X-Forwarded-For would let one caller claim a fresh bucket per request, which
// is the exact opposite of a limit - and the header is written by whoever is
// talking to us.
func TestTheLimiterIgnoresAForwardedForHeader(t *testing.T) {
	srv := newTestServer(t, Config{})
	guess := func(forwarded string) int {
		res := exchangeWith(t, srv, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
			r.Header.Set("X-Forwarded-For", forwarded)
		})
		return res.StatusCode
	}
	for i := range authFailureBurst {
		if got := guess(fmt.Sprintf("10.0.0.%d", i)); got != http.StatusUnauthorized {
			t.Fatalf("guess %d answered %d, want 401", i, got)
		}
	}
	if got := guess("10.0.0.99"); got != http.StatusTooManyRequests {
		t.Errorf("a fresh X-Forwarded-For answered %d, want 429: it bought a new bucket", got)
	}
}

// An empty credential must not compare equal to anything. New never leaves the
// token empty, so this is the guard rather than the hole - but a constant-time
// compare of two empty strings is equal, and that is what it stands in front of.
func TestAnEmptyTokenIsNeverAccepted(t *testing.T) {
	if tokenOK("", "") {
		t.Error("an empty token matched an empty credential")
	}
	if tokenOK("secret", "") {
		t.Error("an empty credential matched a real token")
	}
	if !tokenOK("secret", "secret") {
		t.Error("the right token was refused")
	}
}

// A Host header carries a port, so the branch that splits it has to strip the
// root dot too. The other branch is the portless one, and testing only that
// leaves half the function unpinned.
func TestATrailingDotIsStrippedWithAPortToo(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"aigem.example.ts.net.:7777", "aigem.example.ts.net:7777"},
		{"AIGEM.Example.TS.net.:7777", "aigem.example.ts.net:7777"},
		{"aigem.example.ts.net.", "aigem.example.ts.net"},
		{"127.0.0.1:7777", "127.0.0.1:7777"},
		{"[::1]:7777", "[::1]:7777"},
		// Not a root dot with a name in front of it.
		{".", "."},
		{"", ""},
	} {
		if got := normalizeHost(c.in); got != c.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A bind that is neither loopback nor a wildcard has no local name to keep, so
// with an origin stated it answers to that origin and nothing else - including
// from the machine it runs on. It is a pure function, which is the only way to
// check an address a test cannot bind.
func TestARoutableBindKeepsOnlyTheStatedOrigin(t *testing.T) {
	got, err := hostsFor(&net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 7777},
		[]string{"https://aigem.example.ts.net"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"aigem.example.ts.net"}; !slices.Equal(got.hosts, want) {
		t.Errorf("hosts = %v, want %v", got.hosts, want)
	}
	if want := []string{"https://aigem.example.ts.net"}; !slices.Equal(got.origins, want) {
		t.Errorf("origins = %v, want %v", got.origins, want)
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

// The cap has to be held by Guard, not merely by the counter. Deleting the
// acquire/release from the wrapper leaves the test above green while the daemon
// accepts unlimited sockets, so this one drives Guard itself with real
// handshake headers against a handler that blocks - which is what a websocket
// handler does, for the life of the connection.
func TestTheSocketCapIsHeldByTheGuard(t *testing.T) {
	srv := newTestServer(t, Config{})
	hold := make(chan struct{})
	inside := make(chan struct{}, maxSockets+1)
	var wg sync.WaitGroup
	// The handler blocks until hold closes, so releasing it has to come before
	// anything waits on the goroutines sitting in it - the other order is a test
	// that deadlocks on itself.
	defer func() { close(hold); awaitGroup(t, &wg, "every blocked handshake") }()

	handshake := srv.Guard(func(http.ResponseWriter, *http.Request) {
		inside <- struct{}{}
		<-hold
	})
	dial := func(upgrade bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/fake/socket", nil)
		req.Host = srv.Addr().String()
		req.Header.Set("Authorization", "Bearer "+srv.token)
		if upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		handshake(w, req)
		return w
	}
	occupy := func(upgrade bool) {
		wg.Add(1)
		go func() { defer wg.Done(); dial(upgrade) }()
	}

	for i := range maxSockets {
		occupy(true)
		select {
		case <-inside:
		case <-time.After(5 * time.Second):
			t.Fatalf("handshake %d never reached the handler", i)
		}
	}

	// On a deadline: a daemon that has lost the cap accepts this one and blocks
	// it in the handler forever, and a test that hung would report the defect as
	// a timeout ten minutes later instead of as a failure here.
	past := make(chan *httptest.ResponseRecorder, 1)
	go func() { past <- dial(true) }()
	var res *httptest.ResponseRecorder
	select {
	case res = <-past:
	case <-time.After(5 * time.Second):
		t.Fatal("the socket past the cap was accepted rather than refused")
	}
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("the socket past the cap answered %d, want 503", res.Code)
	}
	if res.Header().Get("Retry-After") == "" {
		t.Error("the refusal does not say when to come back")
	}

	// An ordinary request is not a socket and must not be counted against them.
	occupy(false)
	select {
	case <-inside:
	case <-time.After(5 * time.Second):
		t.Fatal("a plain request was refused while the sockets were full")
	}
}

// The cookie is a sliding window, not a deadline. Reusing a live one stopped
// the exchange walking the table cap, but it also meant a browser in daily use
// was signed out on the thirtieth day with no renewal path - and if that tab
// had ever been closed, the token went with it.
func TestACookieCloseToExpiryIsRenewed(t *testing.T) {
	srv := newTestServer(t, Config{})
	c := exchange(t, srv)

	// Fresh: the exchange keeps it and issues nothing.
	if got := reexchange(t, srv, c); got != "" {
		t.Errorf("a fresh cookie was replaced by %q", got)
	}

	// Inside the renewal window: it is replaced, and the old one stops working.
	srv.mu.Lock()
	srv.cookies[c.Value] = time.Now().Add(renewWithin / 2)
	srv.mu.Unlock()
	next := reexchange(t, srv, c)
	if next == "" {
		t.Fatal("a cookie close to expiry was not renewed")
	}
	if srv.cookieOK(withCookie(t, c)) {
		t.Error("the replaced cookie is still a credential")
	}
	if !srv.cookieOK(withCookie(t, &http.Cookie{Name: cookieName, Value: next})) {
		t.Error("the renewed cookie does not work")
	}
}

// ---- helpers ----

// exchangeWith runs the cookie exchange over real HTTP, so what is tested is
// the route as the mux wires it rather than the handler in isolation.
func exchangeWith(t *testing.T, srv *Server, prepare func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.Base()+"api/auth/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	prepare(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res
}

func exchange(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	res := exchangeWith(t, srv, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+srv.token)
	})
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("the exchange set no cookie")
	return nil
}

// reexchange runs the exchange carrying c, and returns the value of any cookie
// it was given back - empty when the daemon kept the one it had.
func reexchange(t *testing.T, srv *Server, c *http.Cookie) string {
	t.Helper()
	res := exchangeWith(t, srv, func(r *http.Request) { r.AddCookie(c) })
	for _, got := range res.Cookies() {
		if got.Name == cookieName {
			return got.Value
		}
	}
	return ""
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

// A revocation that only cleared the table is undone by the next restart, so
// answering "signed out" when the daemon could not write it would be saying
// something untrue - to exactly the operator who has lost a phone.
func TestLogoutSaysSoWhenItCannotBeRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory anyway")
	}
	dir := t.TempDir()
	srv := newTestServer(t, Config{CookieFile: filepath.Join(dir, "cookies.json")})
	c := exchange(t, srv)

	// The write starts failing after the session exists.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	req, err := http.NewRequest(http.MethodDelete, srv.Base()+"api/auth/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(c)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("a sign-out the daemon could not record answered %d, want 500", res.StatusCode)
	}
}

// Restarting the daemon rotates the token but keeps every session, which is the
// point of keeping them - and means a restart is not how a leaked token is
// remediated. This is.
func TestForgetSessionsRemovesThemAll(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	first := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, first)
	_ = first.Close()

	if err := ForgetSessions(file); err != nil {
		t.Fatalf("ForgetSessions: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the sessions file is still there")
	}

	next := newTestServer(t, Config{CookieFile: file})
	res := exchangeWith(t, next, func(r *http.Request) { r.AddCookie(c) })
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a forgotten session still answered %d, want 401", res.StatusCode)
	}
	// And it is not an error to forget sessions that were never kept.
	if err := ForgetSessions(file); err != nil {
		t.Errorf("ForgetSessions on a missing file: %v", err)
	}
	if err := ForgetSessions(""); err != nil {
		t.Errorf("ForgetSessions with no file: %v", err)
	}
}

// testWait is how this package's tests wait on anything. A regression that
// leaves a goroutine parked should report itself as the test that was waiting,
// with the name of what it was waiting for - not as the whole package timing
// out ten minutes later and taking every other result with it.
const testWait = 10 * time.Second

func awaitGroup(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}
