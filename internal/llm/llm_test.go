package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopDetectorTripsOnRepeatedLines(t *testing.T) {
	var d loopDetector
	line := "Actually, I'll just ask the user.\n"
	tripped := false
	for i := 0; i < repeatLineLimit+2; i++ {
		if d.feed(line) {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatal("expected loop detector to trip on repeated identical lines")
	}
}

func TestLoopDetectorIgnoresBlankSeparators(t *testing.T) {
	var d loopDetector
	// Same line separated by blank lines should still count as repetition. The
	// delta carries all 6 repeats, so feed may report the trip on this one call.
	_ = d.feed("hello\n\nhello\n\nhello\n\nhello\n\nhello\n\nhello\n\n")
	if !d.tripped {
		t.Fatal("blank separators should not reset the repeat counter")
	}
}

func TestLoopDetectorAllowsVariedOutput(t *testing.T) {
	var d loopDetector
	lines := []string{"one\n", "two\n", "three\n", "four\n", "five\n", "six\n", "seven\n"}
	for _, l := range lines {
		if d.feed(l) {
			t.Fatalf("varied lines should not trip the detector")
		}
	}
}

func TestStreamPinsModelTemperature(t *testing.T) {
	var got struct {
		Temperature float64 `json:"temperature"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	one := 1.0
	c := NewClient(ClientConfig{BaseURL: srv.URL, Info: ModelInfo{ID: "m", Temperature: &one}})
	if _, err := c.Stream(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil, 0.3, func(StreamEvent) {}); err != nil {
		t.Fatal(err)
	}
	if got.Temperature != 1.0 {
		t.Fatalf("temperature sent = %v, want pinned 1.0", got.Temperature)
	}

	// Without a pin the caller's value goes through.
	c = NewClient(ClientConfig{BaseURL: srv.URL, Info: ModelInfo{ID: "m"}})
	if _, err := c.Stream(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil, 0.3, func(StreamEvent) {}); err != nil {
		t.Fatal(err)
	}
	if got.Temperature != 0.3 {
		t.Fatalf("temperature sent = %v, want caller's 0.3", got.Temperature)
	}
}

func TestMergeModelInfoTemperature(t *testing.T) {
	one := 1.0
	m := mergeModelInfo(ModelInfo{ID: "m"}, ModelInfo{ID: "m", Temperature: &one})
	if m.Temperature == nil || *m.Temperature != 1.0 {
		t.Fatalf("temperature not merged: %+v", m)
	}
	m = mergeModelInfo(m, ModelInfo{ID: "m"})
	if m.Temperature == nil || *m.Temperature != 1.0 {
		t.Fatalf("temperature lost on merge with empty override: %+v", m)
	}
}
