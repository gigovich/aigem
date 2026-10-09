package runner

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTicketDoneRecordsATrimmedSummaryAndNeedsOne(t *testing.T) {
	var got string
	tool := newTicketDone(func(s string) { got = s })
	if tool.Name() != "ticket_done" || tool.NeedsConfirm() {
		t.Fatalf("name = %q, confirm = %v", tool.Name(), tool.NeedsConfirm())
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"summary":"  "}`)); err == nil || got != "" {
		t.Errorf("an empty summary = %v, recorded %q", err, got)
	}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"summary":" Added the route. "}`))
	if err != nil || out == "" || got != "Added the route." {
		t.Errorf("run = %q, %v, recorded %q", out, err, got)
	}
}
