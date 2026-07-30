package bot

import (
	"strings"
	"testing"
)

func TestMemoryReviewJob(t *testing.T) {
	j := MemoryReviewJob("amiran")
	if j.ID != MemoryReviewJobID {
		t.Fatalf("ID = %q", j.ID)
	}
	if _, err := parseJob(j); err != nil {
		t.Fatalf("expr %q does not parse: %v", j.Expr, err)
	}
	if !strings.Contains(j.Prompt, "memory-hygiene") {
		t.Errorf("prompt must point at the memory-hygiene skill: %q", j.Prompt)
	}
	if j.Expr != MemoryReviewJob("amiran").Expr {
		t.Error("expr must be deterministic per bot name")
	}
	var minutes []string
	for _, name := range []string{"amiran", "lisa", "kate", "jane", "demetre"} {
		minutes = append(minutes, MemoryReviewJob(name).Expr)
	}
	seen := map[string]bool{}
	for _, m := range minutes {
		seen[m] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected staggered minutes across bots, got %v", minutes)
	}
}
