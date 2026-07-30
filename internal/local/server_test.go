package local

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if !Reachable(srv.URL) {
		t.Error("expected reachable")
	}
	srv.Close()
	if Reachable(srv.URL) {
		t.Error("expected not reachable after close")
	}
}

func TestRunningStalePidfile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if Running() {
		t.Fatal("no pidfile yet")
	}
	// Live pid: our own process.
	if err := writePid(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !Running() {
		t.Error("our own pid should be running")
	}
	// Stale pid: an impossible/dead pid; Running cleans it up.
	if err := writePid(1 << 30); err != nil {
		t.Fatal(err)
	}
	if Running() {
		t.Error("dead pid should not be running")
	}
	if _, ok := readPid(); ok {
		t.Error("stale pidfile should have been removed")
	}
}

func TestAssess(t *testing.T) {
	c := Defaults()
	if assess(c, false, func(string) bool { return false }) != ActionNeedsInit {
		t.Error("absent -> NeedsInit")
	}
	if assess(c, true, func(string) bool { return true }) != ActionReady {
		t.Error("reachable -> Ready")
	}
	if assess(c, true, func(string) bool { return false }) != ActionNeedsStart {
		t.Error("unreachable -> NeedsStart")
	}
}

func TestStartStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Stub "llama-server" that just sleeps so the process stays alive.
	stub := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	c.BinaryPath = stub // absolute path; LookPath accepts it
	// Local-file source so waitReady's size lookup never hits the network.
	c.SourceKind = SourcePath
	c.ModelPath = stub
	// Probe: false on the reuse-check, true afterwards (server "healthy").
	calls := 0
	probe := func(string) bool { calls++; return calls > 1 }
	var gotReady bool
	onProgress := func(p Progress) {
		if p.Phase == PhaseReady {
			gotReady = true
		}
	}
	if err := start(context.Background(), c, 5*time.Second, probe, onProgress); err != nil {
		t.Fatal(err)
	}
	if !gotReady {
		t.Error("expected a ready progress callback")
	}
	if !Running() {
		t.Error("expected running after start")
	}
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if Running() {
		t.Error("expected not running after stop")
	}
}

func TestStartReusesReachable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := Defaults()
	c.BinaryPath = "/nonexistent/llama-server" // would fail if we tried to spawn
	// Probe true on the first (reuse) check -> Start returns without spawning.
	if err := start(context.Background(), c, time.Second, func(string) bool { return true }, nil); err != nil {
		t.Fatalf("expected reuse, got %v", err)
	}
}

func TestCacheDirsIncludeOverridesAndHFHub(t *testing.T) {
	t.Setenv("LLAMA_CACHE", "/tmp/llama-cache-test")
	t.Setenv("HF_HUB_CACHE", "/tmp/hf-hub-test")
	dirs := cacheDirs()
	if !slices.Contains(dirs, "/tmp/llama-cache-test") {
		t.Errorf("cacheDirs %v missing LLAMA_CACHE override", dirs)
	}
	if !slices.Contains(dirs, "/tmp/hf-hub-test") {
		t.Errorf("cacheDirs %v missing HF_HUB_CACHE override", dirs)
	}
}

func TestActiveDownloaded(t *testing.T) {
	d := t.TempDir()
	// Pre-existing: a partial from an interrupted attempt (100B) and an unrelated
	// cached model (1000B) that must NOT be counted.
	partial := filepath.Join(d, "model.gguf.incomplete")
	other := filepath.Join(d, "other-model.gguf")
	if err := os.WriteFile(partial, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline := cacheSnapshot([]string{d})

	// Resume: the partial grows to 300B; a new shard (50B) appears.
	if err := os.WriteFile(partial, make([]byte, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "new.gguf"), make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	// Grown file counted at full size (300, not the 200 delta) + new file (50);
	// the unchanged 1000B model is excluded.
	if got := activeDownloaded([]string{d}, baseline); got != 350 {
		t.Errorf("activeDownloaded = %d, want 350", got)
	}
}

func TestHFFileSize(t *testing.T) {
	// Two shards of the matching quant must be summed; the Q8_0 file excluded.
	const body = `[
		{"type":"file","path":"gemma-UD-Q4_K_XL-00001-of-00002.gguf","size":134,"lfs":{"size":4000000000}},
		{"type":"file","path":"gemma-UD-Q4_K_XL-00002-of-00002.gguf","size":134,"lfs":{"size":3000000000}},
		{"type":"file","path":"gemma-Q8_0.gguf","size":134,"lfs":{"size":9000000000}},
		{"type":"file","path":"README.md","size":42}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	n, err := hfFileSize(srv.URL, "me/repo", "UD-Q4_K_XL")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7000000000 {
		t.Errorf("size = %d, want 7000000000 (sum of both matching shards' lfs.size)", n)
	}
	if _, err := hfFileSize(srv.URL, "me/repo", "Q3_K_M"); err == nil {
		t.Error("expected an error when no .gguf matches the quant")
	}
}

func TestFractionAndDetail(t *testing.T) {
	gib := int64(1024 * 1024 * 1024)
	dl := Progress{Phase: PhaseDownloading, Downloaded: 3 * gib, Total: 6 * gib, BytesPerSec: 80 * 1024 * 1024}
	if f := dl.Fraction(); f < 0.49 || f > 0.51 {
		t.Errorf("Fraction = %f, want ~0.5", f)
	}
	if !strings.Contains(dl.Detail(), "/") || !strings.Contains(dl.Detail(), "GiB") {
		t.Errorf("Detail = %q, want sizes with a separator", dl.Detail())
	}
	// Near-complete clamps to 0.99 so the bar fills only on ready.
	if f := (Progress{Phase: PhaseDownloading, Downloaded: 6 * gib, Total: 6 * gib}).Fraction(); f != 0.99 {
		t.Errorf("Fraction at total = %f, want 0.99", f)
	}
	// Completed downloads keep a nearly-full bar while the model loads, and fill on ready.
	if f := (Progress{Phase: PhaseLoading, Downloaded: 6 * gib, Total: 6 * gib}).Fraction(); f != 0.99 {
		t.Errorf("Fraction while loading complete = %f, want 0.99", f)
	}
	if f := (Progress{Phase: PhaseReady, Downloaded: 6 * gib, Total: 6 * gib}).Fraction(); f != 1 {
		t.Errorf("Fraction while ready = %f, want 1", f)
	}
	// No bar when not downloading and incomplete, or when total is unknown.
	if f := (Progress{Phase: PhaseLoading, Downloaded: gib, Total: 6 * gib}).Fraction(); f != -1 {
		t.Errorf("Fraction while loading incomplete = %f, want -1", f)
	}
	if f := (Progress{Phase: PhaseDownloading, Downloaded: gib, Total: 0}).Fraction(); f != -1 {
		t.Errorf("Fraction with unknown total = %f, want -1", f)
	}
	if d := (Progress{Phase: PhaseDownloading, Downloaded: gib, Total: 0, BytesPerSec: 1024}).Detail(); strings.Contains(d, " / ") {
		t.Errorf("Detail with unknown total = %q, should omit the total", d)
	}
}

func TestProgressTracker(t *testing.T) {
	start := time.Unix(1000, 0)
	tr := &progressTracker{total: 1000, lastTime: start}

	p := tr.update(200, start.Add(time.Second)) // 200 bytes downloaded after 1s
	if p.Phase != PhaseDownloading {
		t.Errorf("phase = %v, want downloading", p.Phase)
	}
	if p.Downloaded != 200 || p.BytesPerSec != 200 {
		t.Errorf("got downloaded=%d rate=%d, want 200/200", p.Downloaded, p.BytesPerSec)
	}

	p = tr.update(500, start.Add(2*time.Second)) // +300 in the next 1s
	if p.BytesPerSec != 300 {
		t.Errorf("rate = %d, want 300", p.BytesPerSec)
	}

	p = tr.update(1000, start.Add(3*time.Second)) // downloaded == total -> loading
	if p.Phase != PhaseLoading || p.Downloaded != 1000 {
		t.Errorf("at total: phase=%v downloaded=%d, want loading/1000", p.Phase, p.Downloaded)
	}

	// A resumed partial reports its full size immediately as downloading.
	tr2 := &progressTracker{total: 1000, lastTime: start}
	if p2 := tr2.update(600, start.Add(time.Second)); p2.Phase != PhaseDownloading || p2.Downloaded != 600 {
		t.Errorf("resume: phase=%v downloaded=%d, want downloading/600", p2.Phase, p2.Downloaded)
	}

	// Unknown total, nothing downloaded yet: loading phase, zero progress.
	tr3 := &progressTracker{total: 0, lastTime: start}
	if p3 := tr3.update(0, start.Add(time.Second)); p3.Phase != PhaseLoading || p3.Downloaded != 0 {
		t.Errorf("no-download: phase=%v downloaded=%d, want loading/0", p3.Phase, p3.Downloaded)
	}
}
