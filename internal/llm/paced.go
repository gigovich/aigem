package llm

import (
	"context"
	"time"
)

// streamer is the minimal LLM entrypoint the Paced and Retrying decorators wrap and re-expose.
type streamer interface {
	Stream(ctx context.Context, messages []Message, tools []Tool, temperature float64,
		onEvent func(StreamEvent)) (Message, error)
}

// Paced throttles a caller's LLM request rate: after each completed Stream it
// waits factor times that call's wall-clock duration before returning, so the
// next request cannot start until the pause elapses. factor=1 roughly halves
// throughput (every request is followed by an equal pause); it is used to slow
// unattended bots. A non-positive factor is a passthrough. The sleep yields to
// ctx cancellation, so a turn deadline still cuts it short.
type Paced struct {
	inner  streamer
	factor float64
}

// NewPaced wraps inner so each Stream is followed by a proportional pause.
func NewPaced(inner streamer, factor float64) *Paced {
	return &Paced{inner: inner, factor: factor}
}

func (p *Paced) Stream(ctx context.Context, messages []Message, tools []Tool, temperature float64,
	onEvent func(StreamEvent)) (Message, error) {
	start := time.Now()
	msg, err := p.inner.Stream(ctx, messages, tools, temperature, onEvent)
	if err != nil || p.factor <= 0 {
		return msg, err
	}
	sleepCtx(ctx, time.Duration(p.factor*float64(time.Since(start))))
	return msg, err
}

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
