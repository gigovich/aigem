package chat

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal PNG, so the sniffed type is a real one rather than octet-stream.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func TestPutAttachmentSniffsTheRealBytes(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	// Named as a text file, but the bytes are a PNG. The declared type is not
	// trusted: a renamed non-image riding into a model request used to fail the
	// whole turn with a provider-side invalid_request.
	att, err := s.PutAttachment(ctx, Operator, th.ID, "notes.txt", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if att.Mime != "image/png" {
		t.Fatalf("mime = %q, want image/png sniffed from the bytes", att.Mime)
	}
	if att.Size != int64(len(pngBytes)) {
		t.Fatalf("size = %d, want %d", att.Size, len(pngBytes))
	}

	got, body, err := s.Attachment(ctx, Operator, att.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, pngBytes) {
		t.Fatal("the stored bytes came back different")
	}
	if got.SHA256 != att.SHA256 {
		t.Fatalf("digest changed on read: %q then %q", att.SHA256, got.SHA256)
	}
}

// Content-addressing means the path on disk carries no component the uploader
// chose, so a filename with a slash or a ".." in it cannot become part of it.
func TestAttachmentPathIsContentAddressed(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	att, err := s.PutAttachment(ctx, Operator, th.ID, "../../etc/passwd", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if att.Filename != "passwd" {
		t.Fatalf("filename = %q, want it reduced to its last element", att.Filename)
	}
	for _, name := range []string{`..\..\windows\win.ini`, "..", ".", "", "   "} {
		got := safeFilename(name)
		if strings.ContainsAny(got, `/\`) || got == "." || got == ".." || got == "" {
			t.Fatalf("safeFilename(%q) = %q, which still names a path", name, got)
		}
	}
	want := filepath.Join(s.Dir(), "blobs", att.SHA256[:2], att.SHA256[2:4], att.SHA256)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the blob is not at its content address: %v", err)
	}
	if _, _, err := s.Attachment(ctx, Operator, att.ID); err != nil {
		t.Fatal(err)
	}
}

// The same bytes twice cost one file, and the second upload must not fail on
// the primary key it shares with the first.
func TestUploadingTheSameBytesTwiceIsFree(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	first, err := s.PutAttachment(ctx, Operator, th.ID, "a.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutAttachment(ctx, Operator, th.ID, "b.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("re-uploading identical bytes failed: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("identical bytes got two ids: %q and %q", first.ID, second.ID)
	}
}

func TestAttachmentLimitsAndScope(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	if _, err := s.PutAttachment(ctx, Operator, th.ID, "empty", bytes.NewReader(nil)); err == nil {
		t.Fatal("an empty attachment was accepted")
	}
	oversize := bytes.Repeat([]byte{'x'}, MaxImageBytes+1)
	if _, err := s.PutAttachment(ctx, Operator, th.ID, "big", bytes.NewReader(oversize)); err == nil {
		t.Fatal("an oversized attachment was accepted")
	}
	if _, err := s.PutAttachment(ctx, jane, th.ID, "a.png", bytes.NewReader(pngBytes)); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("an outsider uploaded to a thread: %v, want ErrNoSuchThread", err)
	}

	att, err := s.PutAttachment(ctx, Operator, th.ID, "a.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Attachment(ctx, jane, att.ID); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("an outsider read an attachment: %v, want ErrNoSuchThread", err)
	}
}

// An upload is claimed by the message that carries it, and only then does it
// appear on that message.
func TestAttachmentIsClaimedByItsMessage(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	att, err := s.PutAttachment(ctx, Operator, th.ID, "shot.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Say(ctx, th.ID, Say{
		Author: Operator, Body: "here is the screenshot", Attachments: []string{att.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	on, err := s.AttachmentsOn(ctx, Operator, m.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != 1 || on[0].ID != att.ID {
		t.Fatalf("message %d carries %d attachments, want the one uploaded", m.Seq, len(on))
	}
	page, err := s.Messages(ctx, Operator, th.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) == 0 || len(page[0].Attachments) != 1 || page[0].Attachments[0] != att.ID {
		t.Fatalf("the message page does not list the attachment: %+v", page)
	}
}

// An id from another thread must not be claimable, or a message could carry a
// file its readers are not entitled to.
func TestAMessageCannotClaimAnotherThreadsAttachment(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	mine := mustThread(t, s, "mine", amiran)
	other := mustThread(t, s, "other", demetre)

	att, err := s.PutAttachment(ctx, Operator, other.ID, "shot.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Say(ctx, mine.ID, Say{
		Author: Operator, Body: "not mine to send", Attachments: []string{att.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	on, err := s.AttachmentsOn(ctx, Operator, m.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != 0 {
		t.Fatalf("a message claimed %d attachments from another thread", len(on))
	}
}

func TestSanitizeFieldStripsControlCharacters(t *testing.T) {
	got := SanitizeField("shot\n- forged: line\x00.png")
	if strings.ContainsAny(got, "\n\x00") {
		t.Fatalf("SanitizeField kept a control character: %q", got)
	}
	long := SanitizeField(strings.Repeat("a", 300))
	if len([]rune(long)) > 121 {
		t.Fatalf("SanitizeField returned %d runes, want it capped", len([]rune(long)))
	}
}
