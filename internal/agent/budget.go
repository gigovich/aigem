package agent

import "time"

const (
	// Conservative unattended defaults: generous enough for normal coding turns,
	// but finite so broken read/search/tool loops terminate without operator input.
	DefaultBudgetMaxModelRounds       = 40
	DefaultBudgetMaxToolCalls         = 120
	DefaultBudgetMaxRepeatedToolCalls = 8
	DefaultBudgetMaxDuration          = 20 * time.Minute
)

// TurnBudget bounds one user turn. Zero fields are disabled, which preserves the
// existing unbounded interactive behavior unless a front-end opts in.
type TurnBudget struct {
	MaxModelRounds       int
	MaxToolCalls         int
	MaxRepeatedToolCalls int
	MaxDuration          time.Duration
}

// DefaultTurnBudget returns the safe default for unattended front-ends (-p and bots).
func DefaultTurnBudget() TurnBudget {
	return TurnBudget{
		MaxModelRounds:       DefaultBudgetMaxModelRounds,
		MaxToolCalls:         DefaultBudgetMaxToolCalls,
		MaxRepeatedToolCalls: DefaultBudgetMaxRepeatedToolCalls,
		MaxDuration:          DefaultBudgetMaxDuration,
	}
}
