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
	"time"

	"github.com/gigovich/aigem/internal/config"
)

const (
	inputHistoryLimit    = 500
	inputHistoryMaxBytes = 16 << 20
	historyLockTimeout   = 2 * time.Second
)

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

func loadInputHistory(root string) ([]string, error) {
	path, err := inputHistoryPath(root)
	if err != nil {
		return nil, err
	}
	return loadInputHistoryFile(path, root)
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
		return nil, fmt.Errorf("input history exceeds %d bytes", inputHistoryMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, inputHistoryMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > inputHistoryMaxBytes {
		return nil, fmt.Errorf("input history exceeds %d bytes", inputHistoryMaxBytes)
	}
	var state inputHistoryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Root != root {
		return nil, fmt.Errorf("input history belongs to a different working directory")
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
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		if len(entries) > inputHistoryLimit {
			entries = entries[len(entries)-inputHistoryLimit:]
		}
		return writeInputHistoryFile(path, inputHistoryState{Root: root, Entries: entries})
	})
	return entries, err
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

func withInputHistoryLock(path string, fn func() error) (retErr error) {
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
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
