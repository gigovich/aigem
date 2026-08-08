package tui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestInputHistorySurvivesRestart(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	m := newTestModel(t)
	m.dispatch("/artifacts")
	m.dispatch("/new")

	restarted := newTestModel(t)
	if got, want := restarted.history, []string{"/artifacts", "/new"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("restored history = %#v, want %#v", got, want)
	}

	restarted = step(restarted, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := restarted.input.Value(); got != "/new" {
		t.Fatalf("Up recalled %q, want latest input", got)
	}
	restarted = step(restarted, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := restarted.input.Value(); got != "/artifacts" {
		t.Fatalf("second Up recalled %q, want older input", got)
	}
	restarted = step(restarted, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := restarted.input.Value(); got != "/new" {
		t.Fatalf("Down recalled %q, want newer input", got)
	}

	path, err := inputHistoryPath(m.historyRoot)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("history permissions = %o, want 600", got)
		}
	}
}

func TestInputHistoryTightensExistingFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows privacy is enforced by the parent directory ACL")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if _, err := appendInputHistory(root, "first"); err != nil {
		t.Fatal(err)
	}
	path, err := inputHistoryPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := appendInputHistory(root, "second"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("history permissions = %o after rewrite, want 600", got)
	}
}

func TestInputHistoryIsScopedToWorkingDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	rootA, rootB := t.TempDir(), t.TempDir()
	if _, err := appendInputHistory(rootA, "only A"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendInputHistory(rootB, "only B"); err != nil {
		t.Fatal(err)
	}
	gotA, err := loadInputHistory(rootA)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := loadInputHistory(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 1 || gotA[0] != "only A" || len(gotB) != 1 || gotB[0] != "only B" {
		t.Fatalf("project histories crossed: A=%#v B=%#v", gotA, gotB)
	}
	pathA, _ := inputHistoryPath(rootA)
	pathB, _ := inputHistoryPath(rootB)
	if pathA == pathB {
		t.Fatal("different working directories resolved to the same history file")
	}
}

func TestInputHistoryConcurrentWritersMerge(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, entry := range []string{"first", "second"} {
		wg.Add(1)
		go func(entry string) {
			defer wg.Done()
			<-start
			_, err := appendInputHistory(root, entry)
			errs <- err
		}(entry)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadInputHistory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("concurrent entries were lost: %#v", got)
	}
}

func TestInputHistoryProcessHelper(t *testing.T) {
	mode := os.Getenv("AIGEM_HISTORY_HELPER")
	if mode == "" {
		return
	}
	root := os.Getenv("AIGEM_HISTORY_ROOT")
	switch mode {
	case "append":
		if ready := os.Getenv("AIGEM_HISTORY_READY"); ready != "" {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := appendInputHistory(root, os.Getenv("AIGEM_HISTORY_ENTRY")); err != nil {
			t.Fatal(err)
		}
	case "hold-append", "hold-crash":
		path, err := inputHistoryPath(root)
		if err != nil {
			t.Fatal(err)
		}
		err = withInputHistoryLock(path+".lock", func() error {
			entries, err := loadInputHistoryFile(path, root)
			if err != nil {
				return err
			}
			if err := os.WriteFile(os.Getenv("AIGEM_HISTORY_READY"), []byte("ready"), 0o600); err != nil {
				return err
			}
			if mode == "hold-crash" {
				time.Sleep(30 * time.Second)
				return nil
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(os.Getenv("AIGEM_HISTORY_RELEASE")); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if time.Now().After(deadline) {
					return errors.New("timed out waiting for release")
				}
				time.Sleep(10 * time.Millisecond)
			}
			entries = append(entries, os.Getenv("AIGEM_HISTORY_ENTRY"))
			return writeInputHistoryFile(path, inputHistoryState{Root: root, Entries: entries})
		})
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown history helper mode %q", mode)
	}
}

func cleanupHistoryHelper(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			t.Errorf("kill history helper: %v", killErr)
		}
		if waitErr == nil {
			t.Error("history helper unexpectedly completed during cleanup")
		}
	})
}

func waitForHistoryHelper(t *testing.T, ready string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("history helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInputHistoryConcurrentProcessesMerge(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	signals := t.TempDir()
	ownerReady := filepath.Join(signals, "owner-ready")
	releaseOwner := filepath.Join(signals, "release-owner")
	writerReady := filepath.Join(signals, "writer-ready")

	owner := exec.Command(os.Args[0], "-test.run=^TestInputHistoryProcessHelper$")
	owner.Env = append(os.Environ(),
		"AIGEM_HISTORY_HELPER=hold-append",
		"AIGEM_HISTORY_ROOT="+root,
		"AIGEM_HISTORY_ENTRY=owner",
		"AIGEM_HISTORY_READY="+ownerReady,
		"AIGEM_HISTORY_RELEASE="+releaseOwner)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	cleanupHistoryHelper(t, owner)
	waitForHistoryHelper(t, ownerReady)

	writer := exec.Command(os.Args[0], "-test.run=^TestInputHistoryProcessHelper$")
	writer.Env = append(os.Environ(),
		"AIGEM_HISTORY_HELPER=append",
		"AIGEM_HISTORY_ROOT="+root,
		"AIGEM_HISTORY_ENTRY=writer",
		"AIGEM_HISTORY_READY="+writerReady)
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	cleanupHistoryHelper(t, writer)
	waitForHistoryHelper(t, writerReady)

	// The owner has read the old state but not written it yet. A writer that does
	// not honor the OS lock would publish now and then be overwritten by owner.
	time.Sleep(200 * time.Millisecond)
	if got, err := loadInputHistory(root); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("second process wrote through an OS lock: %#v", got)
	}
	if err := os.WriteFile(releaseOwner, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err != nil {
		t.Fatalf("owner helper failed: %v", err)
	}
	if err := writer.Wait(); err != nil {
		t.Fatalf("writer helper failed: %v", err)
	}
	got, err := loadInputHistory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "owner" || got[1] != "writer" {
		t.Fatalf("concurrent process entries were lost or reordered: %#v", got)
	}
}

func TestInputHistoryLockReleasedWhenProcessDies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestInputHistoryProcessHelper$")
	cmd.Env = append(os.Environ(),
		"AIGEM_HISTORY_HELPER=hold-crash",
		"AIGEM_HISTORY_ROOT="+root,
		"AIGEM_HISTORY_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cleanupHistoryHelper(t, cmd)
	waitForHistoryHelper(t, ready)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed history lock helper exited successfully")
	}
	if _, err := appendInputHistory(root, "after-crash"); err != nil {
		t.Fatalf("OS did not release history lock after process death: %v", err)
	}
}

func TestInputHistoryFailedReplacePreservesPreviousFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if _, err := appendInputHistory(root, "kept"); err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceHistoryFile
	replaceHistoryFile = func(string, string) error { return errors.New("replace failed") }
	t.Cleanup(func() { replaceHistoryFile = oldReplace })
	if _, err := appendInputHistory(root, "lost"); err == nil {
		t.Fatal("append succeeded despite failed atomic replace")
	}
	got, err := loadInputHistory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "kept" {
		t.Fatalf("failed replace damaged prior history: %#v", got)
	}
}

func TestInputHistoryRejectsCorruptAndOversizedFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	path, err := inputHistoryPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInputHistory(root); err == nil {
		t.Fatal("corrupt history was accepted")
	}
	if err := os.Truncate(path, inputHistoryMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInputHistory(root); err == nil {
		t.Fatal("oversized history was accepted")
	}
}

func TestInputHistoryKeepsMostRecentEntries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	path, err := inputHistoryPath(root)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]string, inputHistoryLimit+2)
	for i := range entries {
		entries[i] = string(rune('a' + i%26))
	}
	entries[0], entries[1] = "drop-1", "drop-2"
	if err := writeInputHistoryFile(path, inputHistoryState{Root: root, Entries: entries}); err != nil {
		t.Fatal(err)
	}
	got, err := loadInputHistory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != inputHistoryLimit {
		t.Fatalf("loaded %d entries, want %d", len(got), inputHistoryLimit)
	}
	if got[0] != entries[2] || got[len(got)-1] != entries[len(entries)-1] {
		t.Fatalf("history did not retain the newest entries: first=%q last=%q", got[0], got[len(got)-1])
	}
}

func TestNewShowsInputHistorySaveError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := newTestModel(t)
	oldReplace := replaceHistoryFile
	replaceHistoryFile = func(string, string) error { return errors.New("replace failed") }
	t.Cleanup(func() { replaceHistoryFile = oldReplace })

	m.dispatch("/new")
	if !hasBlock(m, bkNotice, "could not save input history") {
		t.Fatalf("/new discarded the history save error: %#v", m.blocks)
	}
}

func TestInputHistoryFileNameDoesNotExposeWorkingDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "secret-project-name")
	path, err := inputHistoryPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) == filepath.Base(root)+".json" {
		t.Fatalf("history filename exposes working directory: %s", path)
	}
}
