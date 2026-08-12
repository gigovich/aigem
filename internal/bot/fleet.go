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
	role string
	rt   *Runtime
	// actor is the id the store knows this bot by, and part is what answers
	// whether it is in a thread. Together they are the entitlement check that
	// the Mattermost transport could only approximate.
	actor string
	part  Participation
}

// Participation answers, from the one store that decides it, whether an actor
// is in a thread.
//
// Mattermost made this a guess: the recipient asked the chat server with its
// own credentials and fell back to chat when it could not confirm. Here there
// is no second authority to disagree with, so a refusal is final.
type Participation interface {
	IsParticipant(ctx context.Context, thread, actor string) (bool, error)
}

// Member describes a bot joining the roster.
type Member struct {
	Name          string
	Role          string
	Actor         string
	Runtime       *Runtime
	Participation Participation
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[m.Name] = &member{name: m.Name, role: m.Role,
		rt: m.Runtime, actor: m.Actor, part: m.Participation}
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

// Resolve maps a name to the roster name of the bot behind it, or "" when no
// bot in this process answers to it. Names are case-insensitive, so a model
// writing "Kate" reaches the bot "kate".
func (f *Fleet) Resolve(name string) string {
	if f == nil || name == "" {
		return ""
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, m := range f.members {
		if strings.EqualFold(m.name, name) {
			return m.name
		}
	}
	return ""
}

// IsMember reports whether an actor id belongs to a bot in this process. The
// runtime uses it to tell a teammate's ping apart from a person's.
func (f *Fleet) IsMember(actor string) bool {
	if f == nil || actor == "" {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, m := range f.members {
		if m.actor == actor {
			return true
		}
	}
	return false
}

// Deliver hands in to the named teammate's runtime and reports whether it landed.
// False means the name is not local - the caller's chat post is then the only
// path, which is the pre-fleet behaviour and still correct.
//
// Delivery never blocks on the teammate's turn finishing: the runtime queues the
// message the same way an inbound chat event is queued. A synchronous handoff
// would deadlock the moment two bots handed off to each other while both held a
// fleet turn slot.
func (f *Fleet) Deliver(ctx context.Context, to string, in Inbound) bool {
	if f == nil {
		return false
	}
	f.mu.RLock()
	m := f.members[to]
	f.mu.RUnlock()
	if m == nil || m.rt == nil {
		return false
	}
	if !m.entitled(ctx, in) {
		return false
	}
	return m.rt.Enqueue(in)
}

// entitled reports whether this bot is in the thread the message claims to come
// from.
//
// Without it a teammate could wake this bot in any thread the SENDER is in,
// which is a different and larger set - and the woken message is handed
// straight into a running turn, where the model treats it as an instruction to
// act on. The answer comes from the store, which is the only thing that decides
// participation, so a refusal here is final rather than a fallback.
func (m *member) entitled(ctx context.Context, in Inbound) bool {
	if m.part == nil || m.actor == "" {
		return false
	}
	ok, err := m.part.IsParticipant(ctx, string(in.Thread), m.actor)
	return err == nil && ok
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
