package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
//
// A file this package did not write, or one damaged after it did, is reported
// rather than worked around - by every method, writes included. The one
// exception is a final line with no newline, which is a process killed midway
// through an append and is overwritten by the next one. Anything else needs the
// file repaired or removed; removing it restarts the numbering at 1, so a
// client's cursor has to be treated as stale after that.
type Log[T any] struct {
	path   string
	prefix string
	now    func() time.Time

	// seqs and offsets describe every record in the file, in order: seqs[i] is
	// the sequence of the record at offsets[i]. The sequences are read out of the
	// file rather than derived from position, so a file with a gap in them is
	// something this package can refuse rather than silently misread.
	seqs    []int
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

// head is the part of a record this package reads without knowing T. Compaction
// and the index are built out of it, so neither has to decode the payload -
// which is what keeps a record a binary does not fully understand intact.
type head struct {
	Seq int       `json:"seq"`
	At  time.Time `json:"at"`
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
//
// It fails rather than writing without the inter-process lock. See the package
// doc: an append writes at an offset, so two unlocked writers destroy one
// another's records and hand out one sequence twice.
func (l *Log[T]) Append(v T) (Entry[T], error) {
	unlock := lockPath(l.path)
	defer unlock()

	var e Entry[T]
	err := withFileLockStrict(l.path, func() error {
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
	out, err := l.window(since, limit)
	if err == nil {
		return out, nil
	}
	// A peer's Compact renames a new file into place, so an index synced a
	// moment ago can describe an inode that is no longer there and the read
	// fails on offsets that mean nothing. Read once more against whatever is
	// there now; a second failure is the file's, not the race's.
	l.scanned = false
	if err := l.sync(); err != nil {
		return nil, err
	}
	return l.window(since, limit)
}

// Len reports how many records the log still holds.
func (l *Log[T]) Len() (int, error) {
	unlock := lockPath(l.path)
	defer unlock()
	if err := l.sync(); err != nil {
		return 0, err
	}
	return len(l.seqs), nil
}

// Compact rewrites the log without the records older than `before`, and reports
// how many it dropped.
//
// The newest record always survives, even when it is older than the cutoff. An
// empty log would start numbering at 1 again, and the next record would then
// carry a sequence some client is still holding as its cursor - which is a
// resume that silently replays a different history.
//
// The survivors are copied byte for byte. Decoding them into T and marshalling
// them back would rewrite every kept record through whichever version of the
// type this binary happens to hold, so a field written by a newer binary - or
// by one a rollback has replaced - would disappear from history at the next
// trim.
func (l *Log[T]) Compact(before time.Time) (int, error) {
	unlock := lockPath(l.path)
	defer unlock()

	dropped := 0
	err := withFileLockStrict(l.path, func() error {
		if err := l.sync(); err != nil {
			return err
		}
		if len(l.offsets) == 0 {
			return nil
		}
		drop, err := l.dropCount(before)
		if err != nil || drop == 0 {
			return err
		}
		kept, err := l.bytesFrom(l.offsets[drop])
		if err != nil {
			return err
		}
		if err := writeAtomically(l.path, l.prefix, kept); err != nil {
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
	if len(l.seqs) == 0 {
		return 1
	}
	return l.seqs[len(l.seqs)-1] + 1
}

// window decodes the records after `since`, at most limit of them.
func (l *Log[T]) window(since, limit int) ([]Entry[T], error) {
	if len(l.offsets) == 0 {
		return nil, nil
	}
	// A search rather than arithmetic on a first sequence: the sequences come
	// out of the file, and a file whose numbering has a gap must not silently
	// answer with a window shifted by the size of it. The predicate is written
	// against `since` rather than `since+1` so that the largest possible cursor
	// does not overflow into the smallest and return the whole log.
	start := sort.Search(len(l.seqs), func(i int) bool { return l.seqs[i] > since })
	if start >= len(l.offsets) {
		return nil, nil
	}
	end := len(l.offsets)
	if limit > 0 && end-start > limit {
		end = start + limit
	}
	return l.read(start, end)
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
	// From here the bytes may already be on disk, so the index is no longer
	// something this process can vouch for: a failure below is reported, but the
	// record it describes may well be there, and the next call has to find out
	// from the file rather than from what we think we wrote.
	if err := f.Truncate(end); err != nil {
		f.Close()
		l.scanned = false
		return fmt.Errorf("truncate %s: %w", l.path, err)
	}
	// Durable before the index says the record is there.
	if err := f.Sync(); err != nil {
		f.Close()
		l.scanned = false
		return fmt.Errorf("sync %s: %w", l.path, err)
	}
	if err := f.Close(); err != nil {
		l.scanned = false
		return fmt.Errorf("close %s: %w", l.path, err)
	}

	l.offsets = append(l.offsets, l.size)
	l.seqs = append(l.seqs, seq)
	l.size, l.seen = end, end
	if info, err := os.Stat(l.path); err == nil {
		l.mod = info.ModTime()
	} else {
		l.scanned = false
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
				l.path, l.seqs[i], err)
		}
		var e Entry[T]
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse %s: record %d: %w", l.path, l.seqs[i], err)
		}
		out = append(out, e)
	}
	return out, nil
}

// dropCount is how many of the oldest records are older than the cutoff, never
// counting the newest one. It reads only the wrapper, so a payload this binary
// cannot decode is still trimmed correctly.
func (l *Log[T]) dropCount(before time.Time) (int, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	drop := 0
	for drop < len(l.offsets) {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return 0, fmt.Errorf("read %s: record %d ends the file early: %w",
				l.path, l.seqs[drop], err)
		}
		var h head
		if err := json.Unmarshal(line, &h); err != nil {
			return 0, fmt.Errorf("parse %s: record %d: %w", l.path, l.seqs[drop], err)
		}
		if !h.At.Before(before) {
			break
		}
		drop++
	}
	if drop >= len(l.offsets) {
		drop = len(l.offsets) - 1
	}
	return drop, nil
}

// bytesFrom reads the file from off to the end of the last complete record.
func (l *Log[T]) bytesFrom(off int64) ([]byte, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek %s: %w", l.path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, l.size-off))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", l.path, err)
	}
	return data, nil
}

// sync brings the index up to date with the file, rescanning only when the file
// is not the one the index was built from. A peer process appending or
// compacting is what makes that necessary; this process's own writes keep the
// index and the file in step as they go.
//
// Size and mtime together are what "the same file" means here. A peer rewrite
// landing on the same byte count within one mtime tick would go unnoticed, and
// the next append would then truncate at a stale offset - but nothing this
// package does can produce one: an append strictly grows the file, and a
// compaction that would drop nothing returns without writing.
func (l *Log[T]) sync() error {
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		l.seqs, l.offsets, l.size, l.seen = nil, nil, 0, 0
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

// scan rebuilds the index by walking the file one line at a time, reading the
// wrapper of each.
//
// A final line with no newline is a record a process died partway through
// writing. It is left out of the index and out of size, so the next append
// overwrites it - the alternative is a file that can never be written again.
// Anything else that does not parse is reported: the index is what Append
// truncates against, and building one out of a file this package cannot account
// for is how a damaged line becomes lost records.
func (l *Log[T]) scan(info os.FileInfo) error {
	f, err := os.Open(l.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var offsets []int64
	var seqs []int
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("read %s: %w", l.path, err)
			}
			break
		}
		var h head
		if err := json.Unmarshal(line, &h); err != nil {
			return fmt.Errorf("parse %s: the record at byte %d: %w", l.path, off, err)
		}
		switch {
		case h.Seq < 1:
			return fmt.Errorf("parse %s: the record at byte %d has sequence %d", l.path, off, h.Seq)
		case len(seqs) > 0 && h.Seq <= seqs[len(seqs)-1]:
			return fmt.Errorf("parse %s: the record at byte %d has sequence %d, after %d: "+
				"the log is out of order", l.path, off, h.Seq, seqs[len(seqs)-1])
		}
		offsets = append(offsets, off)
		seqs = append(seqs, h.Seq)
		off += int64(len(line))
	}

	l.seqs, l.offsets, l.size, l.seen = seqs, offsets, off, info.Size()
	l.mod, l.scanned = info.ModTime(), true
	return nil
}
