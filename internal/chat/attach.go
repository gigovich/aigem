package chat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxAttachmentBytes bounds one stored file. The number is carried over from
// the Mattermost transport, where it bounded what could be sent to a model:
// base64 inflates a payload by 4/3 and the provider caps the whole request.
//
// Which of these the model may actually look at, and how many, is not decided
// here - that is a fact about a model, and this package does not know what one
// is. The store's only interest is that a conversation cannot be used as
// unbounded disk.
const MaxAttachmentBytes = 3 << 20

// PutAttachment stores a file for a thread and returns its record.
//
// The bytes are content-addressed by sha256, so a re-upload costs nothing and
// the path on disk carries no component the uploader chose - a filename with a
// slash or a "..' in it cannot become part of it.
//
// The declared mime type is not trusted. It is sniffed from the real bytes,
// exactly as the Mattermost transport did, because a renamed non-image that
// rode into a model request used to fail the whole turn with a provider-side
// invalid_request.
func (s *Store) PutAttachment(ctx context.Context, actor, threadID, filename string, r io.Reader) (Attachment, error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return Attachment{}, err
	}
	body, err := io.ReadAll(io.LimitReader(r, MaxAttachmentBytes+1))
	if err != nil {
		return Attachment{}, fmt.Errorf("chat: read attachment: %w", err)
	}
	if len(body) > MaxAttachmentBytes {
		return Attachment{}, invalid("attachment is larger than %s", HumanSize(MaxAttachmentBytes))
	}
	if len(body) == 0 {
		return Attachment{}, invalid("attachment is empty")
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if err := s.writeBlobFile(digest, body); err != nil {
		return Attachment{}, err
	}

	att := Attachment{
		// The id is derived from the thread as well as the bytes. The file on
		// disk is shared by content, but the row is not: thread_id is what the
		// participation check reads, so one row for two threads would mean the
		// second uploader could not see the file they had just uploaded.
		ID:       attachmentID(threadID, digest),
		Thread:   threadID,
		Filename: safeFilename(filename),
		Mime:     http.DetectContentType(body),
		Size:     int64(len(body)),
		SHA256:   digest,
		Created:  s.now(),
	}
	err = s.write(ctx, "put attachment", func(tx *sql.Tx, _ *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO attachments (id, thread_id, filename, mime, size, sha256, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
			att.ID, att.Thread, att.Filename, att.Mime, att.Size, att.SHA256,
			att.Created.UnixMilli())
		return err
	})
	if err != nil {
		return Attachment{}, err
	}
	return att, nil
}

// attachmentID names a file within its thread. Both halves are hashed so the
// id carries no thread id anyone could read off it, and re-uploading the same
// bytes into the same thread lands on the same row.
func attachmentID(threadID, digest string) string {
	sum := sha256.Sum256([]byte(threadID + "\x00" + digest))
	return "a_" + hex.EncodeToString(sum[:])[:24]
}

// writeBlobFile stores content-addressed bytes, fanned out two levels so a
// directory never holds a million entries.
func (s *Store) writeBlobFile(digest string, body []byte) error {
	dir := filepath.Join(s.dir, "blobs", digest[:2], digest[2:4])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("chat: attachment dir: %w", err)
	}
	path := filepath.Join(dir, digest)
	if _, err := os.Stat(path); err == nil {
		return nil // same bytes, already stored
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("chat: attachment temp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chat: write attachment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chat: write attachment: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("chat: secure attachment: %w", err)
	}
	// Rename rather than write in place: a reader must never find a half-written
	// file under a name that promises those exact bytes.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("chat: store attachment: %w", err)
	}
	return nil
}

// Attachment returns a stored file's record and its bytes.
func (s *Store) Attachment(ctx context.Context, actor, id string) (Attachment, []byte, error) {
	var (
		att     Attachment
		created int64
	)
	err := s.r.QueryRowContext(ctx,
		`SELECT a.id, a.thread_id, a.filename, a.mime, a.size, a.sha256, a.created_at
		   FROM attachments a
		   JOIN participants p ON p.thread_id = a.thread_id AND p.actor_id = ?
		  WHERE a.id = ?`, actor, id).
		Scan(&att.ID, &att.Thread, &att.Filename, &att.Mime, &att.Size,
			&att.SHA256, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, nil, ErrNoSuchThread
	}
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("chat: read attachment: %w", err)
	}
	att.Created = time.UnixMilli(created)
	path, err := s.blobPath(att.SHA256)
	if err != nil {
		return Attachment{}, nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("chat: read attachment %s: %w", id, err)
	}
	return att, body, nil
}

// blobPath builds the on-disk path for a digest, refusing anything that is not
// one. The value comes from the database rather than from a request, so this
// guards against a corrupt or tampered row rather than a caller - but the
// alternatives are a panic on a short string and a path that escapes the blob
// root, both inside a request handler.
func (s *Store) blobPath(digest string) (string, error) {
	if !isHex(digest, 64) {
		return "", fmt.Errorf("chat: attachment digest %q is not a sha256", digest)
	}
	return filepath.Join(s.dir, "blobs", digest[:2], digest[2:4], digest), nil
}

// AttachmentsOn returns the files carried by a message.
func (s *Store) AttachmentsOn(ctx context.Context, actor string, messageSeq uint64) ([]Attachment, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT a.id, a.thread_id, a.filename, a.mime, a.size, a.sha256, a.created_at
		   FROM message_attachments ma
		   JOIN attachments a ON a.id = ma.attachment_id
		   JOIN participants p ON p.thread_id = a.thread_id AND p.actor_id = ?
		  WHERE ma.message_seq = ?
		  ORDER BY a.id`, actor, messageSeq)
	if err != nil {
		return nil, fmt.Errorf("chat: read attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.Thread, &a.Filename, &a.Mime, &a.Size,
			&a.SHA256, &created); err != nil {
			return nil, err
		}
		a.Created = time.UnixMilli(created)
		a.Message = messageSeq
		out = append(out, a)
	}
	return out, rows.Err()
}

// SweepBlobs removes attachment files no row refers to any more, and reports
// how many went.
//
// Deleting a thread cascades its attachment rows away but cannot touch the
// filesystem, so without this a "delete" leaves the bytes on disk - and because
// they are content-addressed, re-uploading the same file would silently
// resurrect them. It is a separate call rather than part of DeleteThread
// because the same bytes may still be referenced from another thread, which
// only a full sweep can know.
func (s *Store) SweepBlobs(ctx context.Context) (removed int, err error) {
	live := map[string]bool{}
	rows, err := s.r.QueryContext(ctx, `SELECT DISTINCT sha256 FROM attachments`)
	if err != nil {
		return 0, fmt.Errorf("chat: sweep attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return 0, err
		}
		live[digest] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	root := filepath.Join(s.dir, "blobs")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		// A leftover ".tmp-*" from an interrupted write is not a digest and is
		// nobody's file; it goes too.
		if isHex(name, 64) && live[name] {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("chat: sweep attachments: %w", err)
	}
	return removed, nil
}

// safeFilename reduces an uploaded name to a single harmless label.
//
// Nothing on disk depends on it - the bytes are content-addressed - but the
// name is echoed into the UI and into a Content-Disposition header, and it is
// what a client offers when someone saves the file. So it keeps only the last
// element, with both separators treated as one because a Windows path reaching
// a Linux daemon is still a path.
func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = SanitizeField(name)
	// "." and ".." survive the trim above and name a directory, not a file.
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

// SanitizeField makes a caller-provided string safe to put where a reader will
// take it as runtime-authored: a note to a model, a filename in a header, a
// label in the UI.
//
// It strips three families. Control characters and newlines could forge extra
// lines that look like ours. Bidi overrides are the classic display spoof - a
// U+202E turns "exe.gnp" into "png.exe" in every UI that renders it. Zero-width
// and line/paragraph separators are invisible, so two different strings would
// look identical to whoever has to decide whether to trust one.
func SanitizeField(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f:
			return ' '
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi
			return -1
		case r == 0x200b, r == 0x200c, r == 0x200d, r == 0xfeff: // zero width
			return -1
		case r == 0x2028, r == 0x2029: // line and paragraph separators
			return ' '
		default:
			return r
		}
	}, s)
	s = strings.TrimSpace(s)
	const max = 120
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// HumanSize renders a byte count for a note a model reads.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
