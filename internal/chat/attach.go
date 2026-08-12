package chat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Attachment limits, carried over from the Mattermost transport because the
// reasoning behind them did not change: base64 inflates a payload by 4/3 and
// the provider caps the whole request, so a message may hand the model at most
// a few images and none of them may be large. Anything else is stored and
// described, never sent.
const (
	MaxAttachmentImages = 4
	MaxImageBytes       = 3 << 20
)

// ViewableImageMimes are the attachment types multimodal models accept.
var ViewableImageMimes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

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
	body, err := io.ReadAll(io.LimitReader(r, MaxImageBytes+1))
	if err != nil {
		return Attachment{}, fmt.Errorf("chat: read attachment: %w", err)
	}
	if len(body) > MaxImageBytes {
		return Attachment{}, fmt.Errorf("chat: attachment is larger than %s",
			HumanSize(MaxImageBytes))
	}
	if len(body) == 0 {
		return Attachment{}, errors.New("chat: attachment is empty")
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if err := s.writeBlobFile(digest, body); err != nil {
		return Attachment{}, err
	}

	att := Attachment{
		ID:       "a_" + digest[:16],
		Thread:   threadID,
		Filename: safeFilename(filename),
		Mime:     http.DetectContentType(body),
		Size:     int64(len(body)),
		SHA256:   digest,
		Created:  s.now(),
	}
	err = s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
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
		msg     sql.NullInt64
		created int64
	)
	err := s.r.QueryRowContext(ctx,
		`SELECT a.id, a.thread_id, a.message_seq, a.filename, a.mime, a.size, a.sha256, a.created_at
		   FROM attachments a
		   JOIN participants p ON p.thread_id = a.thread_id AND p.actor_id = ?
		  WHERE a.id = ?`, actor, id).
		Scan(&att.ID, &att.Thread, &msg, &att.Filename, &att.Mime, &att.Size,
			&att.SHA256, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, nil, ErrNoSuchThread
	}
	if err != nil {
		return Attachment{}, nil, err
	}
	att.Created = time.UnixMilli(created)
	if msg.Valid {
		att.Message = uint64(msg.Int64)
	}
	body, err := os.ReadFile(filepath.Join(s.dir, "blobs",
		att.SHA256[:2], att.SHA256[2:4], att.SHA256))
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("chat: read attachment %s: %w", id, err)
	}
	return att, body, nil
}

// AttachmentsOn returns the files carried by a message.
func (s *Store) AttachmentsOn(ctx context.Context, actor string, messageSeq uint64) ([]Attachment, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT a.id, a.thread_id, a.filename, a.mime, a.size, a.sha256, a.created_at
		   FROM attachments a
		   JOIN participants p ON p.thread_id = a.thread_id AND p.actor_id = ?
		  WHERE a.message_seq = ?
		  ORDER BY a.id`, actor, messageSeq)
	if err != nil {
		return nil, err
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

// SanitizeField makes an uploader-provided string safe to put in a note the
// runtime wrote: control characters and newlines - which could forge extra
// lines that look runtime-authored - are stripped, and the length is capped.
func SanitizeField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
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
