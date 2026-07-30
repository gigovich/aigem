package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/config"
)

// startIdleTimeout aborts a launch only after this long with no progress at all
// (no download growth and no health). A large download keeps resetting it, so it
// never caps total wall-clock; it just catches a genuinely stuck server. It also
// has to cover the post-download model load, which shows no progress signal.
const startIdleTimeout = 2 * time.Minute

func pidfilePath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "llama-server.pid"), nil
}

// LogPath returns the path to the llama-server stdout/stderr log file.
func LogPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "llama-server.log"), nil
}

func writePid(pid int) error {
	path, err := pidfilePath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600)
}

func readPid() (int, bool) {
	path, err := pidfilePath()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func removePidfile() error {
	path, err := pidfilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Running reports whether the llama-server process recorded in the pidfile is alive.
func Running() bool {
	pid, ok := readPid()
	if !ok {
		return false
	}
	if !daemonAlive(pid, configuredBinary()) {
		_ = removePidfile()
		return false
	}
	return true
}

// configuredBinary returns the daemon's configured executable, falling back to
// the default name. Load yields a zero Config when local.json is unreadable or
// malformed, and an empty name would match nothing on Windows.
func configuredBinary() string {
	cfg, _, err := Load()
	if err != nil || cfg.BinaryPath == "" {
		return Defaults().BinaryPath
	}
	return cfg.BinaryPath
}

// Reachable returns true when the /health endpoint at baseURL responds 200 OK.
func Reachable(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Phase is the stage of a launch reported through Progress.
type Phase string

// Phases a launch moves through.
const (
	PhaseDownloading Phase = "downloading"
	PhaseLoading     Phase = "loading"
	PhaseReady       Phase = "ready"
)

// Progress reports launch state. Total is 0 when the download size is unknown
// (e.g. a local-file source, or the Hugging Face size lookup failed), in which
// case callers should show bytes/speed without a percentage. The byte fields
// describe the download; on PhaseReady they equal Total when the size was known.
type Progress struct {
	Phase       Phase
	Downloaded  int64 // bytes fetched into the llama.cpp cache since launch started
	Total       int64 // expected total bytes of the model file, or 0 if unknown
	BytesPerSec int64
}

// Fraction returns the download fraction in [0,1] during the download phase, or
// -1 when it cannot be shown as a bar (unknown total, or not downloading).
func (p Progress) Fraction() float64 {
	if p.Phase == PhaseReady && p.Total > 0 {
		return 1
	}
	if p.Phase == PhaseLoading && p.Total > 0 && p.Downloaded >= p.Total {
		return 0.99
	}
	if p.Phase != PhaseDownloading || p.Total <= 0 {
		return -1
	}
	f := float64(p.Downloaded) / float64(p.Total)
	switch {
	case f > 0.99:
		return 0.99 // reserve a full bar for ready (mmproj/rounding can exceed the main file)
	case f < 0:
		return 0
	default:
		return f
	}
}

// Detail returns the size/speed summary without the phase label or percentage,
// e.g. "3.1 GiB / 6.7 GiB · 82.0 MiB/s" (or just "3.1 GiB · 82.0 MiB/s" when the
// total is unknown).
func (p Progress) Detail() string {
	if p.Total > 0 {
		return fmt.Sprintf("%s / %s · %s/s", humanBytes(p.Downloaded), humanBytes(p.Total), humanBytes(p.BytesPerSec))
	}
	return fmt.Sprintf("%s · %s/s", humanBytes(p.Downloaded), humanBytes(p.BytesPerSec))
}

// humanBytes formats a byte count with a binary unit suffix.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Start launches the llama-server daemon for c and waits until it is healthy.
// While an -hf model downloads, onProgress (may be nil) is called about once a
// second with download/load progress; the wait is inactivity-based, so a large
// download never times out as long as it keeps making progress.
func Start(ctx context.Context, c Config, onProgress func(Progress)) error {
	return start(ctx, c, startIdleTimeout, Reachable, onProgress)
}

func start(ctx context.Context, c Config, idle time.Duration, probe func(string) bool, onProgress func(Progress)) error {
	if probe(c.BaseURL()) {
		return nil // already serving; reuse
	}
	if err := c.Validate(); err != nil {
		return err
	}
	logPath, err := LogPath()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()
	_ = os.Chmod(logPath, 0o600) // tighten perms on a pre-existing looser file

	cmd := exec.Command(c.BinaryPath, c.BuildArgs()...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	// New process group so the daemon outlives aigem and Stop can kill its tree.
	cmd.SysProcAttr = detachAttrs()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}
	if err := writePid(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("write pidfile: %w", err)
	}
	_ = cmd.Process.Release() // detach: do not wait, let it run independently
	if !waitReady(ctx, c, idle, probe, onProgress) {
		// Clean up the stale pidfile and kill the stuck process so a retry
		// does not skip Validate and spawn a second instance on the same port.
		_ = Stop()
		return fmt.Errorf("llama-server made no progress for %s; check %s", idle, logPath)
	}
	return nil
}

// waitReady polls until the server is healthy, reporting download/load progress
// and giving up only after idle elapses with no progress (no download growth and
// no health response). Download progress is the combined size of the files the
// launch is actively writing into the model caches (see activeDownloaded).
func waitReady(ctx context.Context, c Config, idle time.Duration, probe func(string) bool, onProgress func(Progress)) bool {
	dirs := cacheDirs()
	baseline := cacheSnapshot(dirs)
	tr := &progressTracker{total: hfTotalSize(c), lastTime: time.Now()}

	lastDownloaded := int64(-1)
	lastChange := time.Now()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if probe(c.BaseURL()) {
			if onProgress != nil {
				onProgress(Progress{Phase: PhaseReady, Downloaded: tr.total, Total: tr.total})
			}
			return true
		}

		downloaded := activeDownloaded(dirs, baseline)
		now := time.Now()
		if downloaded != lastDownloaded {
			lastChange, lastDownloaded = now, downloaded
		}
		if onProgress != nil {
			onProgress(tr.update(downloaded, now))
		}
		if now.Sub(lastChange) > idle {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// progressTracker turns successive downloaded-byte totals into Progress reports,
// computing the transfer rate and phase. It is the only non-trivial bit of the
// wait loop, kept separate so the math is unit-testable.
type progressTracker struct {
	total          int64
	lastDownloaded int64
	lastTime       time.Time
}

// update reports progress for the downloaded-byte total observed at now,
// advancing the tracker's rate state.
func (t *progressTracker) update(downloaded int64, now time.Time) Progress {
	if downloaded < 0 {
		downloaded = 0
	}
	var rate int64
	if dt := now.Sub(t.lastTime).Seconds(); dt > 0 {
		rate = int64(float64(downloaded-t.lastDownloaded) / dt)
		if rate < 0 {
			rate = 0
		}
	}
	phase := PhaseLoading
	if downloaded > 0 && (t.total == 0 || downloaded < t.total) {
		phase = PhaseDownloading
	}
	t.lastDownloaded, t.lastTime = downloaded, now
	return Progress{Phase: phase, Downloaded: downloaded, Total: t.total, BytesPerSec: rate}
}

// cacheDirs returns the directories an -hf launch may write the model into: the
// llama.cpp cache and the Hugging Face hub cache. Recent llama.cpp builds store
// downloads under the HF hub cache, so both are watched. $LLAMA_CACHE,
// $HF_HUB_CACHE and $HF_HOME override the respective defaults.
func cacheDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	add := func(d string) {
		if d != "" {
			dirs = append(dirs, d)
		}
	}

	add(os.Getenv("LLAMA_CACHE"))
	switch {
	case runtime.GOOS == "darwin":
		add(filepath.Join(home, "Library", "Caches", "llama.cpp"))
	case os.Getenv("XDG_CACHE_HOME") != "":
		add(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "llama.cpp"))
	default:
		add(filepath.Join(home, ".cache", "llama.cpp"))
	}

	switch {
	case os.Getenv("HF_HUB_CACHE") != "":
		add(os.Getenv("HF_HUB_CACHE"))
	case os.Getenv("HF_HOME") != "":
		add(filepath.Join(os.Getenv("HF_HOME"), "hub"))
	default:
		add(filepath.Join(home, ".cache", "huggingface", "hub"))
	}
	return dirs
}

// cacheSnapshot records the size of every file under dirs, keyed by path - the
// state of the caches before the launch, so activeDownloaded can tell what this
// launch is fetching apart from models already present.
func cacheSnapshot(dirs []string) map[string]int64 {
	snap := map[string]int64{}
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil {
				snap[p] = info.Size()
			}
			return nil
		})
	}
	return snap
}

// activeDownloaded returns the combined current size of files that are new or
// have grown since baseline - the in-flight download. A grown file is counted at
// its full current size (not just the growth), so a partial left by an
// interrupted attempt and then resumed is reflected correctly.
func activeDownloaded(dirs []string, baseline map[string]int64) int64 {
	var total int64
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if b, ok := baseline[p]; !ok || info.Size() > b {
				total += info.Size()
			}
			return nil
		})
	}
	return total
}

// hfURLBase is the Hugging Face API host, overridable in tests.
var hfURLBase = "https://huggingface.co"

// hfTotalSize returns the byte size of the GGUF an -hf source will download, or
// 0 when the source is a local file or the lookup fails (best-effort).
func hfTotalSize(c Config) int64 {
	if c.SourceKind != SourceHF || c.HFRepo == "" {
		return 0
	}
	repo, quant, _ := strings.Cut(c.HFRepo, ":")
	size, _ := hfFileSize(hfURLBase, repo, quant)
	return size
}

// hfFileSize queries the repo file tree and returns the combined byte size of
// the .gguf files matching quant (or all .gguf files when quant is empty),
// summing across shards like ...-00001-of-00002.gguf. LFS files report the real
// size under lfs.size.
func hfFileSize(apiBase, repo, quant string) (int64, error) {
	url := fmt.Sprintf("%s/api/models/%s/tree/main?recursive=true", strings.TrimRight(apiBase, "/"), repo)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("hf tree %s: status %d", repo, resp.StatusCode)
	}
	var entries []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			Size int64 `json:"size"`
		} `json:"lfs"`
	}
	// The legitimate tree JSON is small; cap the body so a hostile/huge response
	// cannot exhaust memory.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&entries); err != nil {
		return 0, err
	}
	q := strings.ToLower(quant)
	var total int64
	var matched bool
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Path), q) {
			continue
		}
		matched = true
		if e.LFS != nil && e.LFS.Size > 0 {
			total += e.LFS.Size
		} else {
			total += e.Size
		}
	}
	if !matched {
		return 0, fmt.Errorf("no matching .gguf in %s", repo)
	}
	return total, nil
}

// Stop asks the daemon to exit and removes the pidfile. On Unix that is SIGTERM
// to its process group; on Windows there is no catchable signal, so the process
// is terminated outright after its image is matched against the configured
// binary. The configured path is loaded here because a recycled pid must not be
// mistaken for the daemon.
func Stop() error {
	pid, ok := readPid()
	if !ok {
		return nil
	}
	if err := terminateDaemon(pid, configuredBinary()); err != nil {
		// Keep the pidfile: dropping it here would orphan a daemon that is still
		// running and still holding the port, with nothing left to address it by.
		return fmt.Errorf("stop llama-server (pid %d): %w", pid, err)
	}
	return removePidfile()
}

// Report is a snapshot of the daemon's current state, returned by Status.
type Report struct {
	Initialized bool
	Running     bool
	Reachable   bool
	PID         int
	Command     string
	LogPath     string
}

// Status returns a Report snapshot of the current daemon state for config c.
func Status(c Config) Report {
	logPath, _ := LogPath()
	running := Running() // also clears a stale pidfile
	pid := 0
	if running {
		pid, _ = readPid()
	}
	return Report{
		Initialized: Exists(),
		Running:     running,
		Reachable:   Reachable(c.BaseURL()),
		PID:         pid,
		Command:     c.CommandString(),
		LogPath:     logPath,
	}
}

// Action is the outcome of Assess: what the caller must do before using the local model.
type Action int

// Action values returned by Assess.
const (
	ActionReady      Action = iota // reachable; switch immediately
	ActionNeedsInit                // no config; run the wizard
	ActionNeedsStart               // configured but not serving; start it
)

// Assess determines what action is needed before the local model can be used.
func Assess(c Config, exists bool) Action { return assess(c, exists, Reachable) }

func assess(c Config, exists bool, reachable func(string) bool) Action {
	if !exists {
		return ActionNeedsInit
	}
	if reachable(c.BaseURL()) {
		return ActionReady
	}
	return ActionNeedsStart
}
