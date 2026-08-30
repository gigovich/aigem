package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Log is an append-only JSONL document: one record per line, each with a
// sequence number the log assigns and never reuses.
//
// It exists beside File because the two collections the daemon keeps that grow
// without bound - the run records and the activity feed - are read as "what has
// happened since I last looked". Rewriting the whole document to add a line is
// quadratic in the length of the feed, and a whole-document API gives a client
// no cursor to resume from.
//
// The record is wrapped rather than written bare. The wrapper carries the
// sequence and the time, which is what makes a cursor survive compaction: drop
// the oldest records and the survivors keep the numbers a client is already
// holding. A bare record numbered by its position in the file would renumber
// every time the file was trimmed.
type Log[T any] struct {
	path   string
	prefix string
	now    func() time.Time

	// first is the sequence of offsets[0], zero when the log holds nothing.
	// Sequence n therefore lives at offsets[n-first], which is what lets Range
	// seek instead of scanning from the top.
	first   int
	offsets []int64
	// size is the end of the last complete line, which is not the file size when
	// a process died midway through a write. The next append starts there and
	// truncates, so the torn tail is overwritten rather than parsed.
	size int64
	// seen is the file as the index last saw it - its size and its mtime. It is
	// tracked apart from size precisely because a torn tail makes the two
	// differ, and comparing against size would then rescan on every call.
	seen    int64
	mod     time.Time
	scanned bool
}

// Entry is one record with what the log knows about it.
type Entry[T any] struct {
	Seq int       `json:"seq"`
	At  time.Time `json:"at"`
	V   T         `json:"v"`
}

// NewLog returns a log at path. Nothing is read or created until the first
// call, so constructing one never touches the disk.
func NewLog[T any](path string) *Log[T] {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" || stem == "." {
		stem = "log"
	}
	return &Log[T]{path: path, prefix: stem + "-*" + tempSuffix, now: time.Now}
}

// Path reports the log's location.
func (l *Log[T]) Path() string { return l.path }

// Append adds one record and returns it with the sequence and time the log gave
// it. Sequences start at 1 and increase by one.
func (l *Log[T]) Append(v T) (Entry[T], error) {
	unlock := lockPath(l.path)
	defer unlock()

	var e Entry[T]
	err := withFileLock(l.path, func() error {
		// Under the lock, so a peer's appends are in the index before this one
		// picks its sequence and its offset.
		if err := l.sync(); err != nil {
			return err
		}
		e = Entry[T]{Seq: l.nextSeq(), At: l.now().UTC(), V: v}
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode %s: %w", l.path, err)
		}
		return l.write(append(line, '\n'), e.Seq)
	})
	if err != nil {
		return Entry[T]{}, err
	}
	return e, nil
}

// Range returns the records after sequence `since`, at most limit of them; a
// limit of zero or less means all of them. Passing the Seq of the last record a
// client saw is how it resumes.
//
// A cursor older than anything the log still holds is not an error - compaction
// is allowed to have dropped what it points at. The first Seq returned is then
// further along than the caller asked for, which is how a client detects the
// gap.
func (l *Log[T]) Range(since, limit int) ([]Entry[T], error) {
	unlock := lockPath(l.path)
	defer unlock()
	if err := l.sync(); err != nil {
		return nil, err
	}
	if len(l.offsets) == 0 {
		return nil, nil
	}
	start := 0
	if since >= l.first {
		start = since - l.first + 1
	}
	if start >= len(l.offsets) {
		return nil, nil
	}
	end := len(l.offsets)
	if limit > 0 && end-start > limit {
		end = start + limit
	}
	return l.read(start, end)
}

// Len reports how many records the log still holds.
func (l *Log[T]) Len() (int, error) {
	unlock := lockPath(l.path)
	defer unlock()
	if err := l.sync(); err != nil {
		return 0, err
	}
	return len(l.offsets), nil
}

// Compact rewrites the log without the records older than `before`, and reports
// how many it dropped.
//
// The newest record always survives, even when it is older than the cutoff. An
// empty log would start numbering at 1 again, and the next record would then
// carry a sequence some client is still holding as its cursor - which is a
// resume that silently replays a different history.
func (l *Log[T]) Compact(before time.Time) (int, error) {
	unlock := lockPath(l.path)
	defer unlock()

	dropped := 0
	err := withFileLock(l.path, func() error {
		if err := l.sync(); err != nil {
			return err
		}
		if len(l.offsets) == 0 {
			return nil
		}
		all, err := l.read(0, len(l.offsets))
		if err != nil {
			return err
		}
		drop := 0
		for drop < len(all) && all[drop].At.Before(before) {
			drop++
		}
		if drop >= len(all) {
			drop = len(all) - 1
		}
		if drop == 0 {
			return nil
		}
		var buf bytes.Buffer
		for _, e := range all[drop:] {
			line, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("encode %s: %w", l.path, err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if err := writeAtomically(l.path, l.prefix, buf.Bytes()); err != nil {
			return err
		}
		dropped = drop
		// The rename replaced the file the index describes, so it is read again
		// rather than adjusted: the offsets all moved.
		l.scanned = false
		return l.sync()
	})
	return dropped, err
}

func (l *Log[T]) nextSeq() int {
	if len(l.offsets) == 0 {
		return 1
	}
	return l.first + len(l.offsets)
}

// write puts one line at the end of the last complete record. It is a write at
// an offset rather than an O_APPEND, so that a torn tail left by a killed
// process is overwritten instead of being read back forever.
func (l *Log[T]) write(line []byte, seq int) error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	end := l.size + int64(len(line))
	if _, err := f.WriteAt(line, l.size); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	if err := f.Truncate(end); err != nil {
		f.Close()
		return fmt.Errorf("truncate %s: %w", l.path, err)
	}
	// Durable before the index says the record is there. A crash between the two
	// costs the record; a crash the other way round would leave a client resuming
	// from a sequence the file has never held.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", l.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", l.path, err)
	}

	if len(l.offsets) == 0 {
		l.first = seq
	}
	l.offsets = append(l.offsets, l.size)
	l.size, l.seen = end, end
	if info, err := os.Stat(l.path); err == nil {
		l.mod = info.ModTime()
	}
	return nil
}

// read decodes the records at index [start, end) of the current index.
func (l *Log[T]) read(start, end int) ([]Entry[T], error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()
	if _, err := f.Seek(l.offsets[start], io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek %s: %w", l.path, err)
	}
	r := bufio.NewReader(f)
	out := make([]Entry[T], 0, end-start)
	for i := start; i < end; i++ {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read %s: record %d ends the file early: %w",
				l.path, l.first+i, err)
		}
		var e Entry[T]
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse %s: record %d: %w", l.path, l.first+i, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// sync brings the index up to date with the file, rescanning only when the file
// is not the one the index was built from. A peer process appending or
// compacting is what makes that necessary; this process's own writes keep the
// index and the file in step as they go.
func (l *Log[T]) sync() error {
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		l.first, l.offsets, l.size, l.seen = 0, nil, 0, 0
		l.mod, l.scanned = time.Time{}, true
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", l.path, err)
	}
	if l.scanned && info.Size() == l.seen && info.ModTime().Equal(l.mod) {
		return nil
	}
	return l.scan(info)
}

// scan rebuilds the index by walking the file one line at a time.
//
// A final line with no newline is a record a process died partway through
// writing. It is left out of the index and out of size, so the next append
// overwrites it - the alternative is a file that can never be read again.
func (l *Log[T]) scan(info os.FileInfo) error {
	f, err := os.Open(l.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var offsets []int64
	var off int64
	first := 0
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("read %s: %w", l.path, err)
			}
			break
		}
		if first == 0 {
			var e Entry[T]
			if err := json.Unmarshal(line, &e); err != nil {
				return fmt.Errorf("parse %s: first record: %w", l.path, err)
			}
			if e.Seq < 1 {
				return fmt.Errorf("parse %s: first record has sequence %d", l.path, e.Seq)
			}
			first = e.Seq
		}
		offsets = append(offsets, off)
		off += int64(len(line))
	}

	l.first, l.offsets, l.size, l.seen = first, offsets, off, info.Size()
	l.mod, l.scanned = info.ModTime(), true
	return nil
}
