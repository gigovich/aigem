package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The bug this whole file exists for: a browser signed in on a phone was signed
// out by every restart of the daemon, including the ones the operator makes to
// deploy a new binary - and the token that would sign it back in lives in a
// terminal on another machine.
func TestACookieOutlivesTheDaemonThatIssuedIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	first := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, first)

	second := newTestServer(t, Config{CookieFile: file})
	res := exchangeWith(t, second, func(r *http.Request) { r.AddCookie(c) })
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("a cookie from the previous daemon answered %d, want 204", res.StatusCode)
	}
}

func TestAnExpiredEntryIsDroppedOnLoad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	writeCookieFile(t, file, map[string]time.Time{
		"stale": time.Now().Add(-time.Minute),
		"live":  time.Now().Add(cookieTTL),
	})
	srv := newTestServer(t, Config{CookieFile: file})

	// Read before anything touches the table: cookieOK drops an expired entry on
	// its own, so asking it first would pass whether or not the load filtered.
	srv.mu.Lock()
	_, stale := srv.cookies["stale"]
	held := len(srv.cookies)
	srv.mu.Unlock()
	if stale {
		t.Error("an entry that expired before the restart was loaded after it")
	}
	if held != 1 {
		t.Errorf("the daemon loaded %d sessions from a file holding one live one", held)
	}
	if !srv.cookieOK(withCookie(t, &http.Cookie{Name: cookieName, Value: "live"})) {
		t.Error("an entry with time left on it was not loaded")
	}
}

// Stopping the daemon is not a revocation, and the shutdown must not become
// one; saveCookies carries the full argument.
func TestStoppingTheDaemonLeavesTheFileAlone(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	first := exchange(t, srv)
	second := exchange(t, srv)

	_ = srv.Close()
	if _, err := srv.issueCookie(); err != nil {
		t.Fatal(err)
	}

	stored := readCookieFile(t, file)
	for _, c := range []*http.Cookie{first, second} {
		if _, ok := stored[c.Value]; !ok {
			t.Errorf("a session issued before the shutdown is gone from the file: %v", stored)
		}
	}
}

// An expired session is dropped the first time it is presented, and that drop
// is written through: a file that kept it would be a table the daemon has to
// clean out again on every start.
func TestAnExpiredSessionIsDroppedFromTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, srv)

	srv.mu.Lock()
	srv.cookies[c.Value] = time.Now().Add(-time.Second)
	srv.mu.Unlock()
	if srv.cookieOK(withCookie(t, c)) {
		t.Fatal("an expired cookie was accepted")
	}

	if _, ok := readCookieFile(t, file)[c.Value]; ok {
		t.Error("an expired session is still in the file")
	}
}

// The cap bounds the file as well as the table, and the eviction it makes is
// written through - otherwise a restart would restore the sessions it dropped.
func TestTheFileIsCappedWithTheTable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	for range maxCookieSessions + 5 {
		if _, err := srv.issueCookie(); err != nil {
			t.Fatal(err)
		}
	}

	stored := readCookieFile(t, file)
	if len(stored) > maxCookieSessions {
		t.Errorf("the file holds %d sessions, cap is %d", len(stored), maxCookieSessions)
	}
	srv.mu.Lock()
	held := len(srv.cookies)
	srv.mu.Unlock()
	if len(stored) != held {
		t.Errorf("the file holds %d sessions and the daemon %d", len(stored), held)
	}
}

// A renewal is two mutations - the old one goes, a new one arrives - and both
// have to reach the file, or the restart would honour the cookie the browser
// no longer has.
func TestARenewalIsWrittenThrough(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, srv)

	srv.mu.Lock()
	srv.cookies[c.Value] = time.Now().Add(renewWithin / 2)
	srv.mu.Unlock()
	next := reexchange(t, srv, c)
	if next == "" {
		t.Fatal("a cookie close to expiry was not renewed")
	}

	stored := readCookieFile(t, file)
	if _, ok := stored[c.Value]; ok {
		t.Error("the replaced cookie is still in the file")
	}
	if _, ok := stored[next]; !ok {
		t.Error("the renewed cookie was not recorded")
	}
}

// Logging out has to reach the file. A revocation that only cleared the table
// would be undone by the next restart, which is the one thing an operator who
// lost a phone cannot wait out.
func TestLogoutIsWrittenThrough(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, srv)
	if _, ok := readCookieFile(t, file)[c.Value]; !ok {
		t.Fatal("the issued cookie was not recorded")
	}

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
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout answered %d", res.StatusCode)
	}

	if _, ok := readCookieFile(t, file)[c.Value]; ok {
		t.Error("a logged-out session is still in the file, so a restart would honour it again")
	}
}

// A file this daemon cannot parse is not a reason to refuse to serve: the
// operator would be locked out of the UI by the one thing the UI is for.
func TestACorruptFileCostsTheSessionsAndNothingElse(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, Config{CookieFile: file})

	srv.mu.Lock()
	held := len(srv.cookies)
	srv.mu.Unlock()
	if held != 0 {
		t.Errorf("a corrupt file yielded %d sessions", held)
	}
	c := exchange(t, srv)
	if !srv.cookieOK(withCookie(t, c)) {
		t.Error("the daemon cannot sign a browser in after reading a corrupt file")
	}
}

// The file holds live credentials, so it is the same class of secret as the
// token itself.
func TestTheCookieFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	exchange(t, srv)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the cookie file is %o, want 600", perm)
	}
}

// Without a file there is nothing to write to, which is what makes
// TestARestartRevokesEveryCookie the whole story for that configuration. The
// nil store is the mechanism: saveCookies returns on it, so every mutation path
// stays memory-only without any of them having to know that.
func TestWithoutAFileThereIsNothingToWriteTo(t *testing.T) {
	srv := newTestServer(t, Config{})
	if srv.cookieStore != nil {
		t.Error("a daemon with no cookie file holds a store to write to")
	}
	c := exchange(t, srv)
	if !srv.cookieOK(withCookie(t, c)) {
		t.Error("a daemon with no cookie file cannot sign a browser in")
	}
}

// ---- helpers ----

func writeCookieFile(t *testing.T, path string, sessions map[string]time.Time) {
	t.Helper()
	b, err := json.Marshal(cookieFile{Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCookieFile(t *testing.T, path string) map[string]time.Time {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored cookieFile
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("the daemon wrote a file it cannot read back: %v", err)
	}
	return stored.Sessions
}

// Each daemon holds the whole table in memory. Writing that table wholesale
// meant the second one to write erased every session the first had issued - the
// exact failure this file exists to prevent, and reachable from the dev
// workflow's own advice to start a second daemon on a fixed port. The file is
// updated with a change instead, and changes compose.
func TestTwoDaemonsSharingAFileKeepBothSessions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	first := newTestServer(t, Config{CookieFile: file})
	a := exchange(t, first)
	second := newTestServer(t, Config{CookieFile: file})
	b := exchange(t, second)

	stored := readCookieFile(t, file)
	if _, ok := stored[a.Value]; !ok {
		t.Errorf("the first daemon's session was erased by the second: %v", stored)
	}
	if _, ok := stored[b.Value]; !ok {
		t.Errorf("the second daemon's session is missing: %v", stored)
	}

	// And a sign-out removes the one session it was asked to, not the file.
	if err := first.revokeCookie(withCookie(t, a)); err != nil {
		t.Fatal(err)
	}
	stored = readCookieFile(t, file)
	if _, ok := stored[a.Value]; ok {
		t.Error("a revoked session is still in the file")
	}
	if _, ok := stored[b.Value]; !ok {
		t.Errorf("revoking one session took another daemon's with it: %v", stored)
	}
}

// Close empties the table under the same mutex a sign-out reads it with, so a
// request served during shutdown used to find nothing held, return nil, and be
// answered "signed out" while the file kept the session for the next start.
func TestASignOutDuringShutdownSaysItCouldNotBeRecorded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, srv)
	_ = srv.Close()

	if err := srv.revokeCookie(withCookie(t, c)); err == nil {
		t.Error("a sign-out during shutdown reported success")
	}
	if _, ok := readCookieFile(t, file)[c.Value]; !ok {
		t.Error("the session is gone from the file, so there was nothing to report")
	}
}

// A file this daemon cannot parse is one no browser can be signed out of and
// none can be signed in to. The table in memory is the best account of who
// still holds a session, so the update path repairs the file with it rather
// than failing every mutation from then on.
func TestAnUnparseableFileIsRepairedByTheNextChange(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	c := exchange(t, srv)

	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := srv.issueCookie()
	if err != nil {
		t.Fatal(err)
	}

	stored := readCookieFile(t, file)
	for _, want := range []string{c.Value, next} {
		if _, ok := stored[want]; !ok {
			t.Errorf("the repaired file is missing a live session: %v", stored)
		}
	}
}
