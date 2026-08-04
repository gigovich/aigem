package search

import "context"

// LaunchGate caps how many browsers start at once across the whole process.
//
// Chrome is not a resident cost here: it is spawned for one tool call and torn
// down when that call returns. So the thing worth bounding is not how many
// profiles exist but how many browsers are alive at the same moment - which,
// with several bots in one process, is however many of them happen to search
// together. Each bot keeps its own profile (its cookies and logins are its own,
// and sharing one profile would serialize every search in the process behind a
// single Chrome), and this gate bounds the peak instead.
//
// A nil *LaunchGate admits everyone, which is what a single-bot run wants.
type LaunchGate struct {
	sem chan struct{}
}

// NewLaunchGate returns a gate admitting at most n concurrent browsers. n below
// one means no cap.
func NewLaunchGate(n int) *LaunchGate {
	if n < 1 {
		return nil
	}
	return &LaunchGate{sem: make(chan struct{}, n)}
}

// enter blocks until a slot is free or ctx ends. Calling the returned release func more than once
// is a no-op; it is meant to be called from the goroutine that entered.
func (g *LaunchGate) enter(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.sem <- struct{}{}:
		var done bool
		return func() {
			if done {
				return
			}
			done = true
			<-g.sem
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
