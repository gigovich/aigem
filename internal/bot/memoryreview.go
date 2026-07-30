package bot

import (
	"fmt"
	"hash/fnv"
)

// MemoryReviewJobID is the reserved id of the runtime-injected daily memory review job.
const MemoryReviewJobID = "memory-review"

const memoryReviewPrompt = "Daily memory review: load the memory-hygiene skill and perform " +
	"the review exactly as it specifies. Work silently - do not post any message."

// nameOffset derives a stable number below mod from a bot name, so a fleet of bots spreads its
// scheduled work across the hour instead of every bot waking at the same minute.
func nameOffset(botName string, mod uint32) int {
	h := fnv.New32a()
	h.Write([]byte(botName))
	return int(h.Sum32() % mod)
}

// MemoryReviewJob returns the built-in daily review job for a bot. Its minute sits half a slot
// away from the same bot's heartbeat minutes, which is the only way to be sure the two never
// coincide: both derive from the bot name, and simply re-salting the hash still collides by chance.
// A collision meant two fresh agents at once every night, one rewriting the facts the other read.
func MemoryReviewJob(botName string) CronJob {
	minute := (nameOffset(botName, heartbeatOffsetSlots) + heartbeatOffsetSlots/2) % 60
	return CronJob{
		ID:     MemoryReviewJobID,
		Expr:   fmt.Sprintf("%d 3 * * *", minute),
		Prompt: memoryReviewPrompt,
	}
}
