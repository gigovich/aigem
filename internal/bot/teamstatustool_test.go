package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func teamStatus(t *testing.T, self string, f *Fleet) string {
	t.Helper()
	tool := NewTeamStatusTool(self, f)
	if tool == nil {
		t.Fatal("no tool was built")
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTeamStatusReportsWhoIsWorking(t *testing.T) {
	f, rts := fleetWith(t, "amiran", "jane", "kate")
	done := rts["jane"].EnterTurn()
	defer done()

	got := teamStatus(t, "amiran", f)
	// The bot asking must not be in its own roster: it knows what it is doing.
	if strings.Contains(got, "amiran") {
		t.Errorf("the caller is listed among its own teammates: %q", got)
	}
	for _, want := range []string{"jane (tester): working", "kate (tester): idle"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestTeamStatusWithNobodyAlongside(t *testing.T) {
	f, _ := fleetWith(t, "amiran")
	got := teamStatus(t, "amiran", f)
	// A lone bot gets a plain answer, not an empty one: "nobody is there" is what it needs to
	// know before deciding chat is the only way to reach someone.
	if !strings.Contains(got, "no teammates") {
		t.Fatalf("a lone bot should be told so plainly, got %q", got)
	}
}

func TestTeamStatusNeedsARoster(t *testing.T) {
	if NewTeamStatusTool("amiran", nil) != nil {
		t.Fatal("with no roster at all there is nothing to report and no tool to offer")
	}
}
