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
	oversize := bytes.Repeat([]byte{'x'}, MaxAttachmentBytes+1)
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
	m, err := s.Say(ctx, th.ID, Draft{
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
	page, _, _, err := s.Messages(ctx, Operator, th.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) == 0 || len(page[0].Attachments) != 1 || page[0].Attachments[0] != att.ID {
		t.Fatalf("the message page does not list the attachment: %+v", page)
	}
}

// An id from another thread must not be claimable, and the refusal has to be
// loud: accepting the message and dropping the id let the published frame
// advertise a file the stored transcript would never return.
func TestAMessageCannotClaimAnotherThreadsAttachment(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	mine := mustThread(t, s, "mine", amiran)
	other := mustThread(t, s, "other", demetre)

	att, err := s.PutAttachment(ctx, Operator, other.ID, "shot.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{att.ID, "a_nosuchattachment000000"} {
		if _, err := s.Say(ctx, mine.ID, Draft{
			Author: Operator, Body: "not mine to send", Attachments: []string{id},
		}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("claiming %q: %v, want ErrInvalid", id, err)
		}
	}
	msgs, _, _, err := s.Messages(ctx, Operator, mine.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a refused message was still written: %d in the thread", len(msgs))
	}
}

// The same bytes in two threads are two records. One shared row would leave the
// second uploader unable to read the file they had just uploaded, because
// thread_id is what the participation check reads.
func TestTheSameBytesInTwoThreadsAreTwoAttachments(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	mine := mustThread(t, s, "mine", amiran)
	other := mustThread(t, s, "other", demetre)

	first, err := s.PutAttachment(ctx, Operator, mine.ID, "shot.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutAttachment(ctx, Operator, other.ID, "shot.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("both threads share attachment id %q", first.ID)
	}
	if second.Thread != other.ID {
		t.Fatalf("the second upload reports thread %q, want %q", second.Thread, other.ID)
	}
	// demetre is in the second thread only, and must be able to read what was
	// uploaded there.
	if _, _, err := s.Attachment(ctx, demetre, second.ID); err != nil {
		t.Fatalf("a participant could not read their own thread's attachment: %v", err)
	}
	if _, _, err := s.Attachment(ctx, demetre, first.ID); !errors.Is(err, ErrNoSuchThread) {
		t.Fatalf("reading the other thread's copy: %v, want ErrNoSuchThread", err)
	}
	// One file on disk for both, since the bytes are the same.
	if first.SHA256 != second.SHA256 {
		t.Fatal("identical bytes hashed differently")
	}
}

// One upload may legitimately ride on several messages. A single message_seq
// column silently moved the file off the earlier one.
func TestOneAttachmentCanRideOnSeveralMessages(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	att, err := s.PutAttachment(ctx, Operator, th.ID, "shot.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, body := range []string{"here it is", "and again, in context"} {
		m, err := s.Say(ctx, th.ID, Draft{
			Author: Operator, Body: body, Attachments: []string{att.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, m.Seq)
	}
	for _, seq := range seqs {
		on, err := s.AttachmentsOn(ctx, Operator, seq)
		if err != nil {
			t.Fatal(err)
		}
		if len(on) != 1 || on[0].ID != att.ID {
			t.Fatalf("message %d carries %d attachments, want the one uploaded", seq, len(on))
		}
	}
}

// A "delete" that leaves the bytes on disk is not a delete - and because they
// are content-addressed, re-uploading the same file would resurrect them.
func TestSweepBlobsCollectsFilesNoRowRefersTo(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	kept := mustThread(t, s, "kept", amiran)
	doomed := mustThread(t, s, "doomed", demetre)

	if _, err := s.PutAttachment(ctx, Operator, kept.ID, "a.png", bytes.NewReader(pngBytes)); err != nil {
		t.Fatal(err)
	}
	other := append(append([]byte{}, pngBytes...), 'x')
	gone, err := s.PutAttachment(ctx, Operator, doomed.ID, "b.png", bytes.NewReader(other))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread(ctx, Operator, doomed.ID); err != nil {
		t.Fatal(err)
	}

	removed, err := s.SweepBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("swept %d files, want 1", removed)
	}
	path, err := s.blobPath(gone.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the deleted thread's file is still on disk: %v", err)
	}
	// The surviving thread's attachment is untouched and still readable.
	if _, err := s.SweepBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	atts, err := s.Inbox(ctx, Operator, "", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 {
		t.Fatalf("the surviving thread went too: %d left", len(atts))
	}
}

// A corrupt or tampered digest must not panic in a request handler, nor build a
// path that escapes the blob root.
func TestBlobPathRefusesAnythingThatIsNotADigest(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", "ab", "../../etc/passwd", strings.Repeat("z", 64)} {
		if _, err := s.blobPath(bad); err == nil {
			t.Fatalf("blobPath(%q) was accepted", bad)
		}
	}
	good := strings.Repeat("ab", 32)
	path, err := s.blobPath(good)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, filepath.Join(s.Dir(), "blobs")+string(filepath.Separator)) {
		t.Fatalf("blobPath escaped the blob root: %q", path)
	}
}

func TestSanitizeField(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		rule string
	}{
		{"newline", "shot\n- forged: line.png", "\n"},
		{"nul", "shot\x00.png", "\x00"},
		// U+202E is the classic display spoof: it turns "exe.gnp" into
		// "png.exe" in every UI that renders it.
		{"bidi override", "invoice\u202egnp.exe", "\u202e"},
		{"bidi isolate", "a\u2066b\u2069c", "\u2066\u2069"},
		{"zero width", "\ufeffhidden\u200b.png", "\ufeff\u200b"},
		{"line separator", "a\u2028b\u2029c", "\u2028\u2029"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeField(tc.in)
			if strings.ContainsAny(got, tc.rule) {
				t.Fatalf("SanitizeField(%q) = %q, which kept a character it must strip", tc.in, got)
			}
		})
	}
	long := SanitizeField(strings.Repeat("a", 300))
	if len([]rune(long)) > 121 {
		t.Fatalf("SanitizeField returned %d runes, want it capped", len([]rune(long)))
	}
}

// The filename reaches a Content-Disposition header and the UI, so the caller's
// use of SanitizeField is what matters, not the helper in isolation.
func TestPutAttachmentSanitizesTheFilename(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	th := mustThread(t, s, "with a file", amiran)

	att, err := s.PutAttachment(ctx, Operator, th.ID,
		"shot\n- forged: line\u202egnp.exe", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(att.Filename, "\n\u202e") {
		t.Fatalf("stored filename %q still carries a character that forges a line", att.Filename)
	}
}

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"}, {1023, "1023 B"}, {1024, "1 KB"},
		{1 << 20, "1.0 MB"}, {3 << 20, "3.0 MB"},
	} {
		if got := HumanSize(tc.in); got != tc.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
