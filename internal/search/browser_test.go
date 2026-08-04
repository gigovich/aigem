package search

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeLock(t *testing.T, dir, target string) string {
	t.Helper()
	lock := filepath.Join(dir, "SingletonLock")
	if err := os.Symlink(target, lock); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SingletonSocket", "SingletonCookie"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return lock
}

func TestClearStaleProfileLocksDeadOwner(t *testing.T) {
	dir := t.TempDir()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// A pid far above pid_max cannot be a live process.
	writeLock(t, dir, fmt.Sprintf("%s-%d", host, 1<<30))

	if !clearStaleProfileLocks(dir, slog.Default()) {
		t.Fatal("dead-owner locks must be cleared")
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still present after clearing", name)
		}
	}
}

func TestClearStaleProfileLocksKeepsLiveOwner(t *testing.T) {
	dir := t.TempDir()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// Our own pid is definitely alive; the lock must survive.
	lock := writeLock(t, dir, fmt.Sprintf("%s-%d", host, os.Getpid()))

	if clearStaleProfileLocks(dir, slog.Default()) {
		t.Fatal("live-owner locks must not be cleared")
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("live SingletonLock was removed: %v", err)
	}
}

func TestLockProfileSerializesSameDir(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockProfile(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	// A second lock on the same dir must block until the first is released.
	got := make(chan struct{})
	go func() {
		u2, err := lockProfile(context.Background(), dir)
		if err != nil {
			t.Errorf("second lock: %v", err)
			return
		}
		defer u2()
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("second lock never acquired after release")
	}
}

func TestLockProfileDistinctDirsDoNotContend(t *testing.T) {
	u1, err := lockProfile(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer u1()
	// A different dir must not block on the first.
	u2, err := lockProfile(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u2()
}

func TestLockProfileHonorsContext(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockProfile(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := lockProfile(ctx, dir); err == nil {
			t.Error("expected context error while gate is held")
		}
	}()
	cancel()
	wg.Wait()
}

func TestClearStaleProfileLocksForeignOrMissing(t *testing.T) {
	dir := t.TempDir()
	if clearStaleProfileLocks(dir, slog.Default()) {
		t.Fatal("no lock: nothing to clear")
	}
	writeLock(t, dir, "otherhost-12345")
	if clearStaleProfileLocks(dir, slog.Default()) {
		t.Fatal("a foreign-host lock owner cannot be verified dead; keep it")
	}
}
