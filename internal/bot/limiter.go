package bot

import "context"

// TurnLimiter caps how many agent turns run at once across every bot in the
// process. Each bot's Runtime already caps its own threads, but those caps
// multiply: five bots at four workers each is twenty concurrent turns pointed at
// one provider account, which is what earns a 429. The fleet limiter is the only
// place that number is bounded, so scheduled runs take a slot too - a cron job is
// exactly as expensive as a chat turn.
//
// A nil *TurnLimiter is a working no-op, so a single-bot run needs no special case.
type TurnLimiter struct {
	sem chan struct{}
}

// NewTurnLimiter returns a limiter admitting at most n concurrent turns. n below
// one is treated as unlimited, which is what a caller that wants no cap passes.
func NewTurnLimiter(n int) *TurnLimiter {
	if n < 1 {
		return nil
	}
	return &TurnLimiter{sem: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free or ctx ends. Calling the returned release func more than
// once is a no-op rather than a corruption, but it is not safe to call from two goroutines at
// once; every caller releases from the goroutine that acquired. Acquire never returns a nil func
// together with a nil error.
//
// A slot is held for the whole turn, not per model call: the point is to bound
// how many conversations are in flight, and releasing between calls would let
// every bot start a turn and then interleave, which is the state this prevents.
func (l *TurnLimiter) Acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	select {
	case l.sem <- struct{}{}:
		var done bool
		return func() {
			if done {
				return
			}
			done = true
			<-l.sem
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// InFlight is how many turns hold a slot right now, for logging and status.
func (l *TurnLimiter) InFlight() int {
	if l == nil {
		return 0
	}
	return len(l.sem)
}

// Cap is the configured ceiling; zero means unlimited.
func (l *TurnLimiter) Cap() int {
	if l == nil {
		return 0
	}
	return cap(l.sem)
}
