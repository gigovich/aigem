package bot

import (
	"context"
	"testing"
	"time"
)

func TestTurnLimiterNilIsUnlimited(t *testing.T) {
	var l *TurnLimiter
	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("nil limiter must admit: %v", err)
	}
	release()
	if got := l.Cap(); got != 0 {
		t.Fatalf("nil limiter cap = %d, want 0", got)
	}
	if NewTurnLimiter(0) != nil {
		t.Fatal("a cap below one means unlimited, which is the nil limiter")
	}
}

func TestTurnLimiterBoundsConcurrency(t *testing.T) {
	l := NewTurnLimiter(2)
	ctx := context.Background()
	r1, err := l.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.InFlight(); got != 2 {
		t.Fatalf("in flight = %d, want 2", got)
	}

	admitted := make(chan struct{})
	go func() {
		r3, aerr := l.Acquire(ctx)
		if aerr != nil {
			return
		}
		r3()
		close(admitted)
	}()
	select {
	case <-admitted:
		t.Fatal("a third turn was admitted past a cap of two")
	case <-time.After(50 * time.Millisecond):
	}

	r1()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("releasing a slot did not admit the waiter")
	}
	r2()

	// Releasing twice must not free someone else's slot.
	r1()
	if got := l.InFlight(); got != 0 {
		t.Fatalf("in flight after release = %d, want 0", got)
	}
}

func TestTurnLimiterAcquireRespectsContext(t *testing.T) {
	l := NewTurnLimiter(1)
	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("a cancelled context must not wait for a slot")
	}
}
