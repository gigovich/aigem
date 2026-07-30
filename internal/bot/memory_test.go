package bot

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreSaveReadList(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save("Deploy Process", "How prod deploys work", "Run make deploy on main."); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Read("Deploy Process")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []string{"name: Deploy Process", "description: How prod deploys work",
		"Run make deploy on main."} {
		if !strings.Contains(got, want) {
			t.Errorf("Read missing %q in:\n%s", want, got)
		}
	}
	facts, err := s.List()
	if err != nil || len(facts) != 1 || facts[0].Name != "Deploy Process" {
		t.Fatalf("List = %+v, %v", facts, err)
	}
	// Saved under a slugified filename.
	if _, err := os.Stat(filepath.Join(dir, "deploy-process.md")); err != nil {
		t.Errorf("expected deploy-process.md: %v", err)
	}
}

func TestStoreSaveOverwrites(t *testing.T) {
	s := NewStore(t.TempDir())
	_ = s.Save("topic", "first", "body one")
	if err := s.Save("topic", "second", "body two"); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List()
	if len(facts) != 1 || facts[0].Description != "second" {
		t.Fatalf("overwrite failed: %+v", facts)
	}
	got, _ := s.Read("topic")
	if !strings.Contains(got, "body two") || strings.Contains(got, "body one") {
		t.Fatalf("Read after overwrite = %q", got)
	}
}

func TestStoreIndexAndDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if idx, _ := s.Index(); idx != "" {
		t.Fatalf("empty store Index should be \"\", got %q", idx)
	}
	_ = s.Save("alpha", "first fact", "a")
	_ = s.Save("beta", "second fact", "b")
	idx, err := s.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha", "first fact", "beta", "second fact"} {
		if !strings.Contains(idx, want) {
			t.Errorf("Index missing %q in:\n%s", want, idx)
		}
	}
	// MEMORY.md is generated and excluded from List.
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err != nil {
		t.Errorf("MEMORY.md not generated: %v", err)
	}
	if facts, _ := s.List(); len(facts) != 2 {
		t.Fatalf("List should be 2 (MEMORY.md excluded), got %d", len(facts))
	}
	if err := s.Delete("alpha"); err != nil {
		t.Fatal(err)
	}
	idx, _ = s.Index()
	if strings.Contains(idx, "alpha") || !strings.Contains(idx, "beta") {
		t.Fatalf("Index after delete = %q", idx)
	}
}

func TestStoreReadMissing(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Read("nope"); err == nil {
		t.Fatal("Read of missing fact should error")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Deploy Process":   "deploy-process",
		"  Hello, World! ": "hello-world",
		"already-slug":     "already-slug",
		"A//B__C":          "a-b-c",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreConcurrentSaves(t *testing.T) {
	s := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.Save("fact-"+string(rune('a'+n)), "d", "body")
		}(i)
	}
	wg.Wait()
	facts, err := s.List()
	if err != nil || len(facts) != 8 {
		t.Fatalf("concurrent saves: got %d facts, err %v", len(facts), err)
	}
}

func TestStoreBodyWithDashes(t *testing.T) {
	s := NewStore(t.TempDir())
	body := "Intro line.\n---\nSection after a horizontal rule."
	if err := s.Save("doc", "a doc with a rule", body); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read("doc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Section after a horizontal rule.") {
		t.Fatalf("body with --- not preserved on Read:\n%s", got)
	}
	facts, err := s.List()
	if err != nil || len(facts) != 1 {
		t.Fatalf("List = %+v, %v", facts, err)
	}
	if facts[0].Name != "doc" || facts[0].Description != "a doc with a rule" {
		t.Fatalf("frontmatter corrupted by body ---: %+v", facts[0])
	}
}

func TestStoreSlugCollision(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save("Deploy Process", "first", "body one"); err != nil {
		t.Fatal(err)
	}
	// A different name that slugifies to the same file must error, not overwrite.
	if err := s.Save("deploy process", "second", "body two"); err == nil {
		t.Fatal("expected a collision error for a distinct name mapping to the same file")
	}
	// The original fact is intact.
	got, err := s.Read("Deploy Process")
	if err != nil || !strings.Contains(got, "body one") || strings.Contains(got, "body two") {
		t.Fatalf("original fact damaged by collision attempt: %q, %v", got, err)
	}
	// Saving the SAME name still overwrites (legitimate update).
	if err := s.Save("Deploy Process", "updated", "body three"); err != nil {
		t.Fatalf("same-name update should succeed: %v", err)
	}
	got, _ = s.Read("Deploy Process")
	if !strings.Contains(got, "body three") {
		t.Fatalf("same-name update did not apply: %q", got)
	}
}

func TestStoreRemovesIndexWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Save("only", "the only fact", "body")
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err != nil {
		t.Fatalf("MEMORY.md should exist after save: %v", err)
	}
	if err := s.Delete("only"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatalf("MEMORY.md should be gone after deleting the last fact; stat err=%v", err)
	}
}

func TestStoreMetadataLifecycle(t *testing.T) {
	s := NewStore(t.TempDir())
	clock := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	if err := s.Save("topic", "desc", "body one"); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List()
	if facts[0].Modified != "2026-07-21T10:00:00Z" || facts[0].Used != "" || facts[0].Uses != 0 {
		t.Fatalf("after save: %+v", facts[0])
	}

	clock = clock.Add(time.Hour)
	got, err := s.Read("topic")
	if err != nil || !strings.Contains(got, "body one") {
		t.Fatalf("Read = %q, %v", got, err)
	}
	clock = clock.Add(time.Hour)
	if _, err := s.Read("topic"); err != nil {
		t.Fatal(err)
	}
	facts, _ = s.List()
	f := facts[0]
	if f.Modified != "2026-07-21T10:00:00Z" {
		t.Errorf("Read must not touch Modified, got %q", f.Modified)
	}
	if f.Used != "2026-07-21T12:00:00Z" || f.Uses != 2 {
		t.Errorf("after two reads: used=%q uses=%d", f.Used, f.Uses)
	}

	clock = clock.Add(time.Hour)
	if err := s.Save("topic", "desc2", "body two"); err != nil {
		t.Fatal(err)
	}
	facts, _ = s.List()
	f = facts[0]
	if f.Modified != "2026-07-21T13:00:00Z" {
		t.Errorf("Save must refresh Modified, got %q", f.Modified)
	}
	if f.Used != "2026-07-21T12:00:00Z" || f.Uses != 2 {
		t.Errorf("Save must carry usage forward: used=%q uses=%d", f.Used, f.Uses)
	}
	got, _ = s.Read("topic")
	if !strings.Contains(got, "body two") || strings.Contains(got, "body one") {
		t.Fatalf("body after overwrite = %q", got)
	}
}

func TestStoreReadStampsLegacyFact(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "legacy.md")
	content := "---\nname: legacy\ndescription: pre-metadata fact\n---\nold body\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	s.now = func() time.Time { return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC) }

	got, err := s.Read("legacy")
	if err != nil || !strings.Contains(got, "old body") {
		t.Fatalf("Read = %q, %v", got, err)
	}
	facts, _ := s.List()
	f := facts[0]
	if f.Modified != "2026-06-01T08:00:00Z" {
		t.Errorf("legacy Modified must come from pre-rewrite mtime, got %q", f.Modified)
	}
	if f.Used != "2026-07-21T10:00:00Z" || f.Uses != 1 {
		t.Errorf("usage not stamped: %+v", f)
	}
}

func TestStoreArchiveRestore(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Save("keep", "stays", "k")
	_ = s.Save("old", "goes", "old body xyzzy")

	if err := s.Archive("old"); err != nil {
		t.Fatal(err)
	}
	idx, _ := s.Index()
	if strings.Contains(idx, "old") || !strings.Contains(idx, "keep") {
		t.Fatalf("index after archive = %q", idx)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive", "old.md")); err != nil {
		t.Fatalf("archived file: %v", err)
	}
	if _, err := s.Read("old"); err == nil {
		t.Fatal("Read of an archived fact should error")
	}
	if err := s.Archive("old"); err == nil {
		t.Fatal("archiving twice should error")
	}

	if err := s.Restore("old"); err != nil {
		t.Fatal(err)
	}
	idx, _ = s.Index()
	if !strings.Contains(idx, "old") {
		t.Fatalf("index after restore = %q", idx)
	}
	got, err := s.Read("old")
	if err != nil || !strings.Contains(got, "old body xyzzy") || !strings.Contains(got, "description: goes") {
		t.Fatalf("content must survive the archive/restore round-trip: %q, %v", got, err)
	}

	if err := s.Restore("ghost"); err == nil || !strings.Contains(err.Error(), "no archived memory") {
		t.Fatalf("restore of a never-archived name = %v", err)
	}
	_ = s.Archive("old")
	_ = s.Save("old", "recreated", "new o")
	if err := s.Restore("old"); err == nil || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("restore onto an active slug = %v", err)
	}
}

func TestStoreAudit(t *testing.T) {
	s := NewStore(t.TempDir())
	clock := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	out, err := s.Audit()
	if err != nil || !strings.Contains(out, "(no facts)") || !strings.Contains(out, "Archived: (none)") {
		t.Fatalf("empty audit = %q, %v", out, err)
	}

	_ = s.Save("fresh", "read once", "a")
	_ = s.Save("dusty", "never read", "b")
	_ = s.Save("gone", "archived", "c")
	if _, err := s.Read("fresh"); err != nil {
		t.Fatal(err)
	}
	_ = s.Archive("gone")

	clock = clock.Add(72 * time.Hour)
	out, err = s.Audit()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- fresh: modified 3d ago, last used 3d ago, uses 1",
		"- dusty: modified 3d ago, last used never, uses 0",
		"Archived: gone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- gone:") {
		t.Error("archived fact must not appear as an active audit line")
	}
}

func TestStoreArchiveRefusesClobber(t *testing.T) {
	s := NewStore(t.TempDir())
	_ = s.Save("notes", "v1", "first version")
	if err := s.Archive("notes"); err != nil {
		t.Fatal(err)
	}
	_ = s.Save("notes", "v2", "second version")
	if err := s.Archive("notes"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("archiving onto an occupied archive slot = %v", err)
	}
	got, err := s.Inspect("notes")
	if err != nil || !strings.Contains(got, "second version") {
		t.Fatalf("active fact must survive the refused archive: %q, %v", got, err)
	}
}

func TestStoreInspectDoesNotStamp(t *testing.T) {
	s := NewStore(t.TempDir())
	_ = s.Save("quiet", "desc", "quiet body")
	got, err := s.Inspect("quiet")
	if err != nil || !strings.Contains(got, "quiet body") {
		t.Fatalf("Inspect = %q, %v", got, err)
	}
	facts, _ := s.List()
	if facts[0].Used != "" || facts[0].Uses != 0 {
		t.Fatalf("Inspect must not record a use: %+v", facts[0])
	}

	_ = s.Archive("quiet")
	got, err = s.Inspect("quiet")
	if err != nil || !strings.Contains(got, "quiet body") {
		t.Fatalf("Inspect of archived fact = %q, %v", got, err)
	}
	if _, err := s.Inspect("ghost"); err == nil || !strings.Contains(err.Error(), "active or archived") {
		t.Fatalf("Inspect of missing fact = %v", err)
	}
}

func TestStoreSaveOverMalformedResetsUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("not frontmatter at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if err := s.Save("broken", "repaired", "new body"); err != nil {
		t.Fatalf("Save over malformed file: %v", err)
	}
	facts, _ := s.List()
	if facts[0].Uses != 0 || facts[0].Used != "" {
		t.Fatalf("usage must reset when the old file was unparseable: %+v", facts[0])
	}
}

func TestStoreReadWithoutFrontmatterLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	raw := "just a bare note, no frontmatter\n"
	p := filepath.Join(dir, "bare.md")
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	got, err := s.Read("bare")
	if err != nil || got != raw {
		t.Fatalf("Read = %q, %v", got, err)
	}
	onDisk, _ := os.ReadFile(p)
	if string(onDisk) != raw {
		t.Fatalf("file must not be rewritten: %q", onDisk)
	}
}

func TestStoreAuditLegacyMtimeFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "legacy.md")
	content := "---\nname: legacy\ndescription: never read\n---\nbody\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	s.now = func() time.Time { return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC) }
	out, err := s.Audit()
	if err != nil || !strings.Contains(out, "- legacy: modified 2d ago, last used never, uses 0") {
		t.Fatalf("audit = %q, %v", out, err)
	}
}

func TestStoreConcurrentReadsAndSaves(t *testing.T) {
	s := NewStore(t.TempDir())
	_ = s.Save("shared", "desc", "body")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.Read("shared")
		}()
		go func() {
			defer wg.Done()
			_ = s.Save("shared", "desc", "body")
		}()
	}
	wg.Wait()
	got, err := s.Read("shared")
	if err != nil || !strings.Contains(got, "body") {
		t.Fatalf("fact survived concurrency broken: %q, %v", got, err)
	}
}

func TestStoreReadSurvivesStampFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod is bypassed as root")
	}
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Save("f", "desc", "precious body")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	got, err := s.Read("f")
	if err != nil || !strings.Contains(got, "precious body") {
		t.Fatalf("Read must return content when the stamp cannot be written: %q, %v", got, err)
	}
	_ = os.Chmod(dir, 0o755)
	facts, _ := s.List()
	if facts[0].Uses != 0 {
		t.Fatalf("stamp should not have landed: %+v", facts[0])
	}
}

func TestStoreReadPreservesBodyLeadingWhitespace(t *testing.T) {
	s := NewStore(t.TempDir())
	body := "    indented first line\nsecond line"
	if err := s.Save("snippet", "code sample", body); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := s.Read("snippet")
		if err != nil || !strings.Contains(got, "\n    indented first line\n") {
			t.Fatalf("read %d lost the leading indentation: %q, %v", i, got, err)
		}
	}

	dir := t.TempDir()
	raw := "---\nname: spaced\ndescription: blank line after delimiter\n---\n\n# Title\n"
	if err := os.WriteFile(filepath.Join(dir, "spaced.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(dir)
	if _, err := s2.Read("spaced"); err != nil {
		t.Fatal(err)
	}
	got, err := s2.Read("spaced")
	if err != nil || !strings.Contains(got, "---\n\n# Title") {
		t.Fatalf("blank line after frontmatter must survive the rewrite: %q, %v", got, err)
	}
}
