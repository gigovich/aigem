package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeStreamer returns after dur (or when ctx is cancelled), then yields err.
type fakeStreamer struct {
	dur time.Duration
	err error
}

func (f fakeStreamer) Stream(ctx context.Context, _ []Message, _ []Tool, _ float64,
	_ func(StreamEvent)) (Message, error) {
	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case <-time.After(f.dur):
	}
	return Message{Content: "ok"}, f.err
}

func TestPacedProportional(t *testing.T) {
	p := NewPaced(fakeStreamer{dur: 40 * time.Millisecond}, 1.0)
	start := time.Now()
	msg, err := p.Stream(context.Background(), nil, nil, 0, nil)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("content = %q", msg.Content)
	}
	if el < 75*time.Millisecond {
		t.Fatalf("factor 1.0 should ~double a 40ms call, got %v", el)
	}
}

func TestPacedDisabled(t *testing.T) {
	p := NewPaced(fakeStreamer{dur: 40 * time.Millisecond}, 0)
	start := time.Now()
	if _, err := p.Stream(context.Background(), nil, nil, 0, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if el := time.Since(start); el > 70*time.Millisecond {
		t.Fatalf("factor 0 should not pause, got %v", el)
	}
}

func TestPacedNoPauseOnError(t *testing.T) {
	p := NewPaced(fakeStreamer{dur: 40 * time.Millisecond, err: errors.New("boom")}, 1.0)
	start := time.Now()
	_, err := p.Stream(context.Background(), nil, nil, 0, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if el := time.Since(start); el > 70*time.Millisecond {
		t.Fatalf("must not pause after an error, got %v", el)
	}
}

func TestPacedContextCancelCutsPause(t *testing.T) {
	p := NewPaced(fakeStreamer{dur: 10 * time.Millisecond}, 100.0) // would pause ~1s
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.Stream(ctx, nil, nil, 0, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("ctx cancel should cut the pause, got %v", el)
	}
}
