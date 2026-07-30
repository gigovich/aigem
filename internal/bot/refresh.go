package bot

import (
	"context"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
)

// SystemSetter is a Runner whose system prompt can be replaced between turns.
// *agent.Agent satisfies it.
type SystemSetter interface {
	Runner
	SetSystem(string)
}

// RefreshingRunner rebuilds the agent's system prompt from Build before each message, so a
// long-lived thread always runs on the live memory index (facts saved on earlier turns are
// visible on the next one). Conversation history is preserved - SetSystem only swaps the
// system message.
type RefreshingRunner struct {
	Agent SystemSetter
	Build func() string
}

func (r RefreshingRunner) Run(ctx context.Context, input string, ev agent.Events) (string, error) {
	r.Agent.SetSystem(r.Build())
	return r.Agent.Run(ctx, input, ev)
}

// RunWithImages satisfies ImageRunner when the wrapped agent supports image
// input, falling back to a text-only run otherwise.
func (r RefreshingRunner) RunWithImages(ctx context.Context, input string, images []llm.Image,
	ev agent.Events) (string, error) {
	r.Agent.SetSystem(r.Build())
	if ir, ok := r.Agent.(ImageRunner); ok {
		return ir.RunWithImages(ctx, input, images, ev)
	}
	return r.Agent.Run(ctx, input, ev)
}
