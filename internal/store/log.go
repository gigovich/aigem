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
//
// A gap in the numbering is read, not refused: a cursor lands correctly either
// side of it. What is refused is a sequence that does not increase, because
// that is a file whose order this package cannot reason about.
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
	// seen is the file as the index last saw it: the stat itself, plus its size
	// broken out because a torn tail makes that differ from l.size. The stat is
	// kept whole so os.SameFile can tell a rewrite from an append - a compaction
	// renames a new inode into place, which size and mtime alone can miss.
	seen     int64
	seenInfo os.FileInfo
	scanned  bool
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
//
// A non-zero count with a non-nil error means the rewrite landed and the index
// could not be rebuilt afterwards. The records are gone from the file either
// way; the next call reads the index again.
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
	// sync ran under this same lock a moment ago, so l.size is at or before the
	// end of the file. Checked anyway, because the one thing a wrong offset here
	// does is not refuse - it writes past the end, and the hole that leaves is
	// the only state in this package that no later call can recover from.
	if info, serr := f.Stat(); serr == nil && info.Size() < l.size {
		f.Close()
		l.scanned = false
		return fmt.Errorf("write %s: the index ends at byte %d and the file at %d; "+
			"it was truncated underneath this write", l.path, l.size, info.Size())
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
	//
	// The truncate is insurance rather than a step this package's own writes
	// need. What it removes is a torn tail longer than the record replacing it,
	// and a torn tail is by definition unterminated, so scan would treat the
	// remainder as one too. It costs nothing and it is the difference between a
	// file with a stray suffix and one without.
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
	// The stat this index will be compared against next time. A failure here is
	// not a failed append - the record is on disk - so the index is dropped and
	// the next call reads the file instead.
	if info, err := os.Stat(l.path); err == nil {
		l.seenInfo = info
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
		// The offset came from an index that may describe a file a peer has since
		// replaced. Without this the read returns whatever happens to sit at that
		// byte - a window of real records, shifted, which a client accepts and
		// then advances its cursor past. Range's retry only fires on an error, so
		// the mismatch has to be one.
		if e.Seq != l.seqs[i] {
			return nil, fmt.Errorf("read %s: record %d is at the offset the index gave "+
				"for record %d; the file changed underneath it", l.path, e.Seq, l.seqs[i])
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
// "The same file" is os.SameFile first, then size and mtime. The identity check
// is what catches a compaction, which renames a new inode into place and can
// land on the same byte count within one mtime tick - and the cost of missing
// that is not a stale read but an append truncating at an offset from a file
// that is gone. Size and mtime then catch what happens to the same inode: an
// append, or a rewrite in place that kept the length.
//
// A file that only grew is indexed from where the last scan stopped rather than
// from the top: the alternative is parsing every record of a feed on every
// request that touches it, which on a few megabytes is tens of milliseconds.
func (l *Log[T]) sync() error {
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		l.reset()
		l.scanned = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", l.path, err)
	}
	// os.SameFile is documented to return false for anything it did not produce,
	// which covers l.seenInfo being nil - the state the missing-file branch above
	// leaves behind. The short circuit is what keeps the mtime read below from
	// reaching a nil.
	if !l.scanned || !os.SameFile(info, l.seenInfo) {
		return l.scan(info, 0)
	}
	if info.Size() == l.seen && info.ModTime().Equal(l.seenInfo.ModTime()) {
		return nil
	}
	if info.Size() == l.seen {
		// Same inode, same length, different mtime: something rewrote it in place.
		// The offsets may still be right and the numbering may not, so the index
		// is built again rather than trusted.
		return l.scan(info, 0)
	}
	if info.Size() > l.seen {
		// Extending the index trusts the bytes before l.size unread. The record
		// at the seam is checked by scan's ordering guard; this checks the one
		// the index ends on, which is what catches a prefix rewritten in place
		// under an inode that also grew. It is a spot check, not a verification:
		// a rewrite that left this record alone and changed an earlier one would
		// still get past, and the alternative is parsing the whole file on every
		// call to a growing feed.
		if !l.lastRecordStillThere() {
			return l.scan(info, 0)
		}
		return l.scan(info, l.size)
	}
	// Shrunk without being replaced: something truncated the file under us. The
	// index describes bytes that are no longer there, so it is built again.
	return l.scan(info, 0)
}

// lastRecordStillThere reports whether the record the index ends on still reads
// back with the sequence the index has for it.
func (l *Log[T]) lastRecordStillThere() bool {
	if len(l.offsets) == 0 {
		return true
	}
	last := len(l.offsets) - 1
	f, err := os.Open(l.path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(l.offsets[last], io.SeekStart); err != nil {
		return false
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return false
	}
	var h head
	return json.Unmarshal(line, &h) == nil && h.Seq == l.seqs[last]
}

func (l *Log[T]) reset() {
	l.seqs, l.offsets, l.size, l.seen = nil, nil, 0, 0
	l.seenInfo, l.scanned = nil, false
}

// scan indexes the file from byte `from`, keeping whatever the index already
// holds up to that point. from is zero for a file this index cannot vouch for,
// and l.size for one that has only grown since the last scan.
//
// A final line with no newline is a record a process died partway through
// writing. It is left out of the index and out of size, so the next append
// overwrites it - the alternative is a file that can never be written again.
// Anything else that does not parse is reported: the index is what Append
// truncates against, and building one out of a file this package cannot account
// for is how a damaged line becomes lost records.
func (l *Log[T]) scan(info os.FileInfo, from int64) error {
	f, err := os.Open(l.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()

	offsets, seqs := l.offsets, l.seqs
	if from == 0 {
		offsets, seqs = nil, nil
	} else if _, err := f.Seek(from, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", l.path, err)
	}

	r := bufio.NewReader(f)
	off := from
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
			l.reset()
			return fmt.Errorf("parse %s: the record at byte %d: %w", l.path, off, err)
		}
		switch {
		case h.Seq < 1:
			l.reset()
			return fmt.Errorf("parse %s: the record at byte %d has sequence %d", l.path, off, h.Seq)
		case len(seqs) > 0 && h.Seq <= seqs[len(seqs)-1]:
			// Also the guard on an incremental scan: a file that grew but is not
			// the one the index describes cannot continue its numbering, so it is
			// caught here rather than indexed as if it did.
			l.reset()
			return fmt.Errorf("parse %s: the record at byte %d has sequence %d, after %d: "+
				"the log is out of order", l.path, off, h.Seq, seqs[len(seqs)-1])
		}
		offsets = append(offsets, off)
		seqs = append(seqs, h.Seq)
		off += int64(len(line))
	}

	l.seqs, l.offsets, l.size, l.seen = seqs, offsets, off, info.Size()
	l.seenInfo, l.scanned = info, true
	return nil
}
