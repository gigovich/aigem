package search

import (
	"context"
	"testing"
	"time"
)

func TestLaunchGateNilAdmitsEveryone(t *testing.T) {
	var g *LaunchGate
	leave, err := g.enter(context.Background())
	if err != nil {
		t.Fatalf("a nil gate must admit: %v", err)
	}
	leave()
	if NewLaunchGate(0) != nil {
		t.Fatal("a cap below one means no cap, which is the nil gate")
	}
}

func TestLaunchGateBoundsConcurrentBrowsers(t *testing.T) {
	g := NewLaunchGate(1)
	ctx := context.Background()
	first, err := g.enter(ctx)
	if err != nil {
		t.Fatal(err)
	}

	admitted := make(chan struct{})
	go func() {
		second, aerr := g.enter(ctx)
		if aerr != nil {
			return
		}
		second()
		close(admitted)
	}()
	select {
	case <-admitted:
		t.Fatal("a second browser started past a cap of one")
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("releasing the slot did not admit the waiter")
	}

	// A double release must not hand out a slot nobody holds; if it did, the cap would drift
	// upward over a long run and stop bounding anything.
	first()
	blocked, err := g.enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked()
	if len(g.sem) != 1 {
		t.Fatalf("gate holds %d slots, want 1", len(g.sem))
	}
}

func TestLaunchGateEnterRespectsContext(t *testing.T) {
	g := NewLaunchGate(1)
	leave, err := g.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.enter(ctx); err == nil {
		t.Fatal("a cancelled turn must not sit waiting for a browser slot")
	}
}

func TestChainRunsBothCleanupsInOrder(t *testing.T) {
	var order []string
	chain(func() { order = append(order, "first") }, func() { order = append(order, "second") })()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("cleanup order = %v", order)
	}
}
