package mattermost

import (
	"sync"
	"time"
)

// bufMsg is one observed channel message the bot was not addressed in.
type bufMsg struct {
	author string
	text   string
	at     time.Time
}

// channelBuffer keeps a bounded, time-limited ring of recent messages per channel.
type channelBuffer struct {
	mu        sync.Mutex
	cap       int
	ttl       time.Duration
	now       func() time.Time
	byChannel map[string][]bufMsg
}

func newChannelBuffer(capacity int, ttl time.Duration) *channelBuffer {
	return &channelBuffer{
		cap:       capacity,
		ttl:       ttl,
		now:       time.Now,
		byChannel: map[string][]bufMsg{},
	}
}

func (b *channelBuffer) add(channelID, author, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := append(b.byChannel[channelID], bufMsg{author: author, text: text, at: b.now()})
	if len(msgs) > b.cap {
		msgs = append([]bufMsg(nil), msgs[len(msgs)-b.cap:]...)
	}
	b.byChannel[channelID] = msgs
}

// recent returns the non-expired messages for a channel, oldest first, pruning expired
// entries from the store as a side effect.
func (b *channelBuffer) recent(channelID string) []bufMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := b.now().Add(-b.ttl)
	src := b.byChannel[channelID]
	out := make([]bufMsg, 0, len(src))
	for _, m := range src {
		if m.at.After(cutoff) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		delete(b.byChannel, channelID)
	} else {
		b.byChannel[channelID] = out
	}
	return out
}
