package bot

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Fleet is the roster of bots running in this process and the path a message
// between two of them takes without leaving it.
//
// Two things it buys that chat alone cannot. A handoff reaches a teammate even
// while the websocket is down or reconnecting, because delivery no longer
// depends on the chat server pushing the post back to us. And a bot can see that
// a teammate is mid-turn before pinging them again, which is the difference
// between waiting and piling on.
//
// The chat post is still written: it is the record a human reads. Local delivery
// only removes the wait for that post to come back around. Both copies carry the
// same PostID, and the Runtime drops whichever arrives second, so a teammate acts
// once no matter which path wins the race.
//
// A nil *Fleet is a working no-op: a lone bot has no teammates to reach.
type Fleet struct {
	mu      sync.RWMutex
	members map[string]*member
}

type member struct {
	name string
	// username is the bot's chat username, which is what a teammate addresses. It is resolved
	// from the chat server rather than assumed equal to name: delivering by a name the chat
	// server does not agree with would route someone else's direct message into this bot.
	username string
	role     string
	rt       *Runtime
	// resolver answers, with this bot's own credentials, which channels it belongs to. It is what
	// keeps in-process delivery inside the same boundary chat enforces.
	resolver ChannelResolver
	userID   string
}

// Member describes a bot joining the roster.
type Member struct {
	Name     string
	Username string
	Role     string
	UserID   string
	Runtime  *Runtime
	Resolver ChannelResolver
}

// MemberStatus is one teammate as the roster reports it.
type MemberStatus struct {
	Name string
	Role string
	Busy bool
}

// NewFleet returns an empty roster.
func NewFleet() *Fleet { return &Fleet{members: map[string]*member{}} }

// Register adds a bot to the roster. Registering a name twice replaces the entry, so a bot that
// somehow registers without having unregistered cannot leave a dead runtime on the roster.
func (f *Fleet) Register(m Member) {
	if f == nil || m.Name == "" {
		return
	}
	username := m.Username
	if username == "" {
		username = m.Name // the chat server could not be asked; the name is the best guess left
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[m.Name] = &member{name: m.Name, username: username, role: m.Role,
		rt: m.Runtime, resolver: m.Resolver, userID: m.UserID}
}

// Unregister drops a bot from the roster, so a teammate that stopped is not
// reported as reachable and no message is delivered into its dead runtime.
func (f *Fleet) Unregister(name string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.members, name)
}

// Has reports whether name is a bot running in this process.
func (f *Fleet) Has(name string) bool { return f.Resolve(name) != "" }

// Resolve maps a chat username to the roster name of the bot behind it, or "" when no bot in this
// process answers to it.
//
// Matching is on the CHAT USERNAME, not the aigem name: the caller addressed a chat account, and
// a bot whose aigem name happens to equal some person's username must not receive that person's
// messages. Chat usernames are case-insensitive, so "@Kate" reaches the account "kate".
func (f *Fleet) Resolve(username string) string {
	if f == nil || username == "" {
		return ""
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, m := range f.members {
		if strings.EqualFold(m.username, username) {
			return m.name
		}
	}
	return ""
}

// IsMember reports whether a chat user id belongs to a bot in this process. The runtime uses it
// to tell a teammate's ping apart from a person's.
func (f *Fleet) IsMember(userID string) bool {
	if f == nil || userID == "" {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, m := range f.members {
		if m.userID == userID {
			return true
		}
	}
	return false
}

// MarkRouted tells a teammate that these chat posts have already been handled, so the copies the
// chat server pushes back are ignored. It is how a message split across several posts wakes them
// once rather than once per chunk.
func (f *Fleet) MarkRouted(to string, postIDs []string) {
	if f == nil || len(postIDs) == 0 {
		return
	}
	f.mu.RLock()
	m := f.members[to]
	f.mu.RUnlock()
	if m == nil || m.rt == nil {
		return
	}
	for _, id := range postIDs {
		m.rt.alreadyRouted(id)
	}
}

// Deliver hands in to the named teammate's runtime and reports whether it landed.
// False means the name is not local - the caller's chat post is then the only
// path, which is the pre-fleet behaviour and still correct.
//
// Delivery never blocks on the teammate's turn finishing: the runtime queues the
// message the same way an inbound chat event is queued. A synchronous handoff
// would deadlock the moment two bots handed off to each other while both held a
// fleet turn slot.
func (f *Fleet) Deliver(ctx context.Context, to, channel string, in Inbound) bool {
	if f == nil {
		return false
	}
	f.mu.RLock()
	m := f.members[to]
	f.mu.RUnlock()
	if m == nil || m.rt == nil {
		return false
	}
	if !m.entitled(ctx, channel, in) {
		return false
	}
	return m.rt.Enqueue(in)
}

// entitled reports whether this message would have reached the bot over chat anyway.
//
// This is the check the websocket performs for free and the in-process path does not: a bot only
// ever sees posts in channels it belongs to. Without it a teammate could wake this bot in any
// channel the SENDER can post to, which is a different and larger set - and the woken message is
// handed straight into a running turn, where the model treats it as an instruction to act on.
// So the recipient asks, with its own credentials, whether it belongs to the channel the message
// claims to come from; anything it cannot confirm falls back to chat, which is what decided this
// before the fleet existed.
func (m *member) entitled(ctx context.Context, channel string, in Inbound) bool {
	if in.Kind == "dm" {
		// A direct message is addressed to this bot's own account by name, and Resolve has already
		// confirmed the name is this bot's chat username, so the conversation is one it is in.
		return true
	}
	if m.resolver == nil || channel == "" {
		return false
	}
	id, err := m.resolver.ResolveChannel(ctx, channel)
	return err == nil && id == in.Channel
}

// Roster returns every teammate, sorted by name, with whether each is mid-turn.
func (f *Fleet) Roster() []MemberStatus {
	if f == nil {
		return nil
	}
	// Snapshot the members, then ask each whether it is busy with the roster lock released:
	// Busy takes that bot's own lock, and blocking every other bot's delivery behind one slow
	// runtime is exactly the coupling one process must not introduce.
	f.mu.RLock()
	members := make([]*member, 0, len(f.members))
	for _, m := range f.members {
		members = append(members, m)
	}
	f.mu.RUnlock()
	out := make([]MemberStatus, 0, len(members))
	for _, m := range members {
		out = append(out, MemberStatus{Name: m.name, Role: m.role, Busy: m.busy()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// busy reports whether this member is mid-turn, tolerating a member with no runtime.
func (m *member) busy() bool { return m != nil && m.rt != nil && m.rt.Busy() }

// Busy reports whether the named teammate is running a turn. A name that is not
// local reports false: nothing is known about it, and claiming it is busy would
// stop a caller from doing the one thing that can reach it.
func (f *Fleet) Busy(name string) bool {
	if f == nil {
		return false
	}
	f.mu.RLock()
	m := f.members[name]
	f.mu.RUnlock()
	return m.busy()
}
