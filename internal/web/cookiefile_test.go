package web

import (
	"encoding/json"
	"fmt"
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
	before := readCookieFile(t, file)
	if _, err := srv.issueCookie(); err != nil {
		t.Fatal(err)
	}

	stored := readCookieFile(t, file)
	for _, c := range []*http.Cookie{first, second} {
		if _, ok := stored[c.Value]; !ok {
			t.Errorf("a session issued before the shutdown is gone from the file: %v", stored)
		}
	}
	// And the one issued after it is not there. Close empties the table and then
	// keeps serving until the listener is shut, so a page reloading through a
	// deploy would otherwise record a session nothing is going to honour.
	if len(stored) != len(before) {
		t.Errorf("the file holds %d sessions after a shutdown issue, held %d before",
			len(stored), len(before))
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

// A file that does not parse is the one failure a read-modify-write can never
// get past, so it is started over. Started over from the change alone, though,
// and never from the table in memory: an id in that table may be one a peer
// revoked, and writing it back would undo the sign-out.
func TestAnUnparseableFileIsStartedAgainFromTheChange(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	old := exchange(t, srv)

	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := srv.issueCookie()
	if err != nil {
		t.Fatal(err)
	}

	stored := readCookieFile(t, file)
	if _, ok := stored[next]; !ok {
		t.Errorf("the change that repaired the file is not in it: %v", stored)
	}
	// The earlier session is gone from the file, and that is the point: it was
	// unreadable from a broken file anyway, and rewriting the whole table is how
	// a revoked session comes back.
	if _, ok := stored[old.Value]; ok {
		t.Error("the repair wrote back a session the change did not touch")
	}
	// It still works here until this daemon stops, which is all it was worth.
	if !srv.cookieOK(withCookie(t, old)) {
		t.Error("the repair dropped a live session from memory as well")
	}
}

// A file this daemon cannot read - a permissions problem, a peer holding the
// lock, a full disk - is still whatever it was. Overwriting it there destroys a
// perfectly good file in answer to a problem that has nothing to do with its
// contents.
//
// The file is made unreadable and left in a writable directory, because that is
// the shape where the difference shows: the update fails and a rewrite would
// succeed.
func TestAFileThatCannotBeReadIsNotOverwritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a write-only file anyway")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "cookies.json")
	srv := newTestServer(t, Config{CookieFile: file})
	first := exchange(t, srv)

	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o200); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.issueCookie(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the file was rewritten after a failure that had nothing to do with it:\n%s", after)
	}
	if _, ok := readCookieFile(t, file)[first.Value]; !ok {
		t.Error("the session that was safely on disk is gone")
	}
}

// The cap bounds the table, not the file two daemons share. Recording an
// eviction as a removal meant one daemon deleting another's live sessions,
// picked by an expiry order that has nothing to do with which daemon owns them.
func TestTheCapDoesNotEvictFromTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	peer := map[string]time.Time{}
	for i := range maxCookieSessions + 10 {
		// Staggered, so the cap has an order to evict by rather than a tie.
		peer[fmt.Sprintf("peer-%03d", i)] = time.Now().Add(cookieTTL + time.Duration(i)*time.Minute)
	}
	writeCookieFile(t, file, peer)

	srv := newTestServer(t, Config{CookieFile: file})
	// Loading is where the cap bounds what a shared file can grow to, now that
	// eviction no longer writes through.
	srv.mu.Lock()
	held := len(srv.cookies)
	srv.mu.Unlock()
	if held != maxCookieSessions {
		t.Errorf("the daemon loaded %d sessions from a file holding %d, cap is %d",
			held, len(peer), maxCookieSessions)
	}

	mine := exchange(t, srv)
	stored := readCookieFile(t, file)
	if _, ok := stored[mine.Value]; !ok {
		t.Error("the session this daemon issued is not in the file")
	}
	var gone int
	for id := range peer {
		if _, ok := stored[id]; !ok {
			gone++
		}
	}
	if gone != 0 {
		t.Errorf("issuing one session removed %d of a peer's from the file", gone)
	}
}

// A daemon answers for what it loaded at startup and issued since, so a session
// a peer issued is in the file and not in this table. Returning early there was
// a sign-out that reported success and revoked nothing.
func TestSigningOutASessionThisDaemonNeverIssued(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cookies.json")
	first := newTestServer(t, Config{CookieFile: file})
	second := newTestServer(t, Config{CookieFile: file})
	// Issued after the first daemon read the file, so only the second knows it.
	c := exchange(t, second)
	first.mu.Lock()
	_, known := first.cookies[c.Value]
	first.mu.Unlock()
	if known {
		t.Fatal("the first daemon knows the session; this test proves nothing")
	}

	if err := first.revokeCookie(withCookie(t, c)); err != nil {
		t.Fatalf("revokeCookie: %v", err)
	}
	if _, ok := readCookieFile(t, file)[c.Value]; ok {
		t.Error("the sign-out reported success and left the session in the file")
	}
}

// Carrying on past a failed removal hands the browser a cookie the daemon may
// not have recorded, while the one it replaced stays live on disk: the page is
// signed out by the next restart and a session nobody holds outlives it.
func TestARenewalStopsWhenTheReplacementCannotBeRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory anyway")
	}
	dir := t.TempDir()
	srv := newTestServer(t, Config{CookieFile: filepath.Join(dir, "cookies.json")})
	c := exchange(t, srv)

	// Inside the renewal window, so the exchange replaces rather than reuses.
	srv.mu.Lock()
	srv.cookies[c.Value] = time.Now().Add(renewWithin / 2)
	srv.mu.Unlock()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	res := exchangeWith(t, srv, func(r *http.Request) { r.AddCookie(c) })
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("a renewal that could not be recorded answered %d, want 500", res.StatusCode)
	}
	for _, got := range res.Cookies() {
		if got.Name == cookieName {
			t.Errorf("the browser was handed a replacement cookie %q anyway", got.Value)
		}
	}
}
