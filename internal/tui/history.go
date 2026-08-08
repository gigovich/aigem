package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/config"
)

const (
	inputHistoryLimit = 500
	// One recalled entry is something you scroll back to with an arrow key, so a
	// pasted document is not one. Capping the entry keeps a single paste from
	// sitting in the file forever and being re-read on every submission.
	inputHistoryMaxEntryBytes = 16 << 10
	inputHistoryMaxBytes      = 16 << 20
	historyLockTimeout        = 2 * time.Second
	// Temp files are removed on every failure path, so one on disk means a
	// process died mid-write. Old enough that a live writer's file is never hit.
	inputHistoryTempMaxAge = time.Hour
)

// errUnusableHistory marks a history file that cannot be read back but is also
// not worth an error: corrupt, oversized, or left by another directory that
// happened to collide. It is a recall cache, so the answer is to start over
// rather than to disable saving and complain on every submission.
var errUnusableHistory = errors.New("input history file is unusable")

type inputHistoryState struct {
	Root    string   `json:"root"`
	Entries []string `json:"entries"`
}

func inputHistoryPath(root string) (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "input-history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := secureInputHistoryDir(dir); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

// loadInputHistory reads the recall list for root. An unusable file reads as an
// empty history: startup must not be interrupted, and must not open with a
// notice block either - one would displace the welcome screen for good.
func loadInputHistory(root string) ([]string, error) {
	path, err := inputHistoryPath(root)
	if err != nil {
		return nil, err
	}
	entries, err := loadInputHistoryFile(path, root)
	if errors.Is(err, errUnusableHistory) {
		return nil, nil
	}
	return entries, err
}

func loadInputHistoryFile(path, root string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > inputHistoryMaxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", errUnusableHistory, inputHistoryMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, inputHistoryMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > inputHistoryMaxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", errUnusableHistory, inputHistoryMaxBytes)
	}
	var state inputHistoryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: %w", errUnusableHistory, err)
	}
	if state.Root != root {
		return nil, fmt.Errorf("%w: belongs to a different working directory", errUnusableHistory)
	}
	if len(state.Entries) > inputHistoryLimit {
		state.Entries = state.Entries[len(state.Entries)-inputHistoryLimit:]
	}
	return state.Entries, nil
}

// appendInputHistory serializes writers across processes and re-reads while the
// lock is held, so two TUI instances cannot overwrite each other's new entries.
func appendInputHistory(root, entry string) ([]string, error) {
	path, err := inputHistoryPath(root)
	if err != nil {
		return nil, err
	}
	var entries []string
	err = withInputHistoryLock(path+".lock", func() error {
		var err error
		entries, err = loadInputHistoryFile(path, root)
		if errors.Is(err, errUnusableHistory) {
			// Overwrite it rather than refusing to save from here on.
			entries, err = nil, nil
		}
		if err != nil {
			return err
		}
		sweepInputHistoryTemps(filepath.Dir(path))
		entries = append(entries, entry)
		if len(entries) > inputHistoryLimit {
			entries = entries[len(entries)-inputHistoryLimit:]
		}
		return writeInputHistoryFile(path, inputHistoryState{Root: root, Entries: entries})
	})
	return entries, err
}

// sweepInputHistoryTemps removes temp files a killed writer left behind. They
// hold prompts, and nothing else ever prunes the directory. Best effort: a temp
// that cannot be removed is not worth failing a save over.
func sweepInputHistoryTemps(dir string) {
	names, err := filepath.Glob(filepath.Join(dir, ".history-*.json.tmp"))
	if err != nil {
		return
	}
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil || time.Since(info.ModTime()) < inputHistoryTempMaxAge {
			continue
		}
		_ = os.Remove(name)
	}
}

func writeInputHistoryFile(path string, state inputHistoryState) (retErr error) {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > inputHistoryMaxBytes {
		return fmt.Errorf("input history exceeds %d bytes", inputHistoryMaxBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceHistoryFile(tmpName, path)
}

var replaceHistoryFile = replaceInputHistoryFile

// recordsInputHistory reports whether an submitted line is worth recalling.
// A slash command is not: /new is the last thing most sessions see, and it
// would be the first thing the next one offers on Up. Neither is a bare Enter
// sent with an attached image, nor a paste far too long to arrow back to.
func recordsInputHistory(input string) bool {
	return strings.TrimSpace(input) != "" &&
		!strings.HasPrefix(input, "/") &&
		len(input) <= inputHistoryMaxEntryBytes
}

func withInputHistoryLock(path string, fn func() error) (retErr error) {
	// O_NOFOLLOW where the platform has it: the lock file sits in a directory
	// this process created, but a symlink planted there would otherwise have its
	// target chmod'd below.
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|openNoFollow, 0o600)
	if err != nil {
		return err
	}
	locked := false
	defer func() {
		var unlockErr error
		if locked {
			unlockErr = unlockInputHistoryFile(lock)
		}
		retErr = errors.Join(retErr, unlockErr, lock.Close())
	}()
	if err := lock.Chmod(0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(historyLockTimeout)
	for {
		ok, err := tryLockInputHistoryFile(lock)
		if err != nil {
			return err
		}
		if ok {
			locked = true
			return fn()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for input history lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
