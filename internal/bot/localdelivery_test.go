package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeParticipation answers the entitlement check the fleet makes before
// handing a message into another bot's runtime.
type fakeParticipation struct {
	in  map[string]bool // thread -> the actor is in it
	err error
}

func (f fakeParticipation) IsParticipant(_ context.Context, thread, _ string) (bool, error) {
	return f.in[thread], f.err
}

// fleetWith registers the named bots, each with its own runtime, for tests
// about the roster rather than about entitlement.
func fleetWith(t *testing.T, names ...string) (*Fleet, map[string]*Runtime) {
	t.Helper()
	f := NewFleet()
	rts := map[string]*Runtime{}
	for _, n := range names {
		rt, _ := fleetRuntime(t)
		rts[n] = rt
		f.Register(Member{Name: n, Actor: "bot:" + n, Role: "tester", Runtime: rt,
			Participation: allParticipation{}})
	}
	return f, rts
}

// deliveryFleet registers one teammate whose runtime records what it received.
func deliveryFleet(t *testing.T, name string, in map[string]bool) (*Fleet, *Runtime, chan Inbound) {
	t.Helper()
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	rt := NewRuntime(ft, func(string) Runner { return nil }, 4)
	got := rt.enqueued

	fleet := NewFleet()
	fleet.Register(Member{
		Name: name, Actor: "bot:" + name, Runtime: rt,
		Participation: fakeParticipation{in: in},
	})
	return fleet, rt, got
}

// The message is written to the thread first and its failure is the tool's
// failure: a message delivered in-process but missing from the thread would be
// invisible to the operator reading along.
func TestHandoffWritesTheThreadBeforeDelivering(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	f.sayErr = errors.New("chat: no such thread")
	fleet, _, got := deliveryFleet(t, "demetre", map[string]bool{"t_0102030405060708": true})
	local := &LocalDelivery{Self: "amiran", SelfActor: "bot:amiran", Fleet: fleet}

	_, err := NewHandoffTool(f, local, "amiran").Run(context.Background(),
		json.RawMessage(`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`))
	if err == nil {
		t.Fatal("a handoff whose write failed reported success")
	}
	select {
	case in := <-got:
		t.Fatalf("the teammate was woken by a message that was never written: %+v", in)
	default:
	}
}

func TestHandoffDeliversLocallyAndStillWritesTheThread(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	fleet, _, got := deliveryFleet(t, "demetre", map[string]bool{"t_0102030405060708": true})
	local := &LocalDelivery{Self: "amiran", SelfActor: "bot:amiran", Fleet: fleet}

	out, err := NewHandoffTool(f, local, "amiran").Run(context.Background(),
		json.RawMessage(`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "woke them directly") {
		t.Fatalf("result does not say the teammate was woken: %q", out)
	}
	if len(f.said) != 1 {
		t.Fatalf("wrote %d messages, want the one the operator reads", len(f.said))
	}
	select {
	case in := <-got:
		if in.Kind != "mention" || in.Thread != "t_0102030405060708" {
			t.Fatalf("delivered %+v", in)
		}
		// The author is the sending bot, so the receiver resolves the sender
		// exactly as it would for the same message arriving from the store.
		if in.Author != "bot:amiran" {
			t.Fatalf("author = %q", in.Author)
		}
	default:
		t.Fatal("the teammate was not woken")
	}
}

// A name no bot in this process answers to is not a failure: the store's own
// fan-out still reaches them, which is what decided this before the fleet
// existed.
func TestHandoffToANonLocalTeammateStillWrites(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	fleet, _, got := deliveryFleet(t, "jane", map[string]bool{"t_0102030405060708": true})
	local := &LocalDelivery{Self: "amiran", SelfActor: "bot:amiran", Fleet: fleet}

	out, err := NewHandoffTool(f, local, "amiran").Run(context.Background(),
		json.RawMessage(`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "woke them directly") {
		t.Fatalf("claimed to wake a teammate that is not in this process: %q", out)
	}
	if len(f.said) != 1 {
		t.Fatalf("wrote %d messages, want one", len(f.said))
	}
	select {
	case in := <-got:
		t.Fatalf("jane was woken by a handoff to demetre: %+v", in)
	default:
	}
}

// Without the entitlement check a teammate could wake this bot in any thread
// the SENDER is in, and the woken message is handed straight into a running
// turn where the model treats it as an instruction.
func TestLocalDeliveryStaysInsideParticipation(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	// demetre is not in the thread the handoff claims to come from.
	fleet, _, got := deliveryFleet(t, "demetre", map[string]bool{})
	local := &LocalDelivery{Self: "amiran", SelfActor: "bot:amiran", Fleet: fleet}

	out, err := NewHandoffTool(f, local, "amiran").Run(context.Background(),
		json.RawMessage(`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "woke them directly") {
		t.Fatalf("delivered into a thread the teammate is not in: %q", out)
	}
	select {
	case in := <-got:
		t.Fatalf("a non-participant was woken: %+v", in)
	default:
	}
	// The message is still written, so a teammate who joins later can read it.
	if len(f.said) != 1 {
		t.Fatalf("wrote %d messages, want one", len(f.said))
	}
}

// A store that cannot answer is not a yes. There is no second authority to fall
// back to any more, so an unconfirmed delivery is refused.
func TestLocalDeliveryRefusesWhenParticipationCannotBeChecked(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	rt := NewRuntime(ft, func(string) Runner { return nil }, 4)
	fleet := NewFleet()
	fleet.Register(Member{Name: "demetre", Actor: "bot:demetre", Runtime: rt,
		Participation: fakeParticipation{err: errors.New("database is closed")}})

	if fleet.Deliver(context.Background(), "demetre",
		Inbound{Kind: "mention", Thread: "t_0102030405060708"}) {
		t.Fatal("delivery succeeded while participation could not be checked")
	}
}

// A member with no way to check is not entitled either: registering without one
// must not open a hole.
func TestLocalDeliveryRefusesAMemberWithNoParticipationCheck(t *testing.T) {
	ft := &fakeTransport{in: make(chan Inbound, 4)}
	rt := NewRuntime(ft, func(string) Runner { return nil }, 4)
	fleet := NewFleet()
	fleet.Register(Member{Name: "demetre", Actor: "bot:demetre", Runtime: rt})

	if fleet.Deliver(context.Background(), "demetre",
		Inbound{Kind: "mention", Thread: "t_0102030405060708"}) {
		t.Fatal("a member with no participation check accepted a delivery")
	}
}

func TestLocalDeliveryNeverTargetsItself(t *testing.T) {
	fleet, _, _ := deliveryFleet(t, "amiran", map[string]bool{"t_0102030405060708": true})
	local := &LocalDelivery{Self: "amiran", SelfActor: "bot:amiran", Fleet: fleet}

	if got := local.target("amiran"); got != "" {
		t.Fatalf("target(self) = %q, want none", got)
	}
	if got := local.target("AMIRAN"); got != "" {
		t.Fatalf("target is case-sensitive about itself: %q", got)
	}
}

func TestFleetResolvesTeammateNamesCaseInsensitively(t *testing.T) {
	fleet, _, _ := deliveryFleet(t, "demetre", nil)
	for _, name := range []string{"demetre", "Demetre", "DEMETRE"} {
		if got := fleet.Resolve(name); got != "demetre" {
			t.Fatalf("Resolve(%q) = %q", name, got)
		}
	}
	if got := fleet.Resolve("ghost"); got != "" {
		t.Fatalf("Resolve of an unknown name = %q", got)
	}
}

// A nil fleet is a lone bot, and everything on this path has to stay a working
// no-op for it.
func TestLocalDeliveryIsANoOpWithNoFleet(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	var local *LocalDelivery

	out, err := NewHandoffTool(f, local, "amiran").Run(context.Background(),
		json.RawMessage(`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "woke them directly") {
		t.Fatalf("a lone bot claimed to wake someone: %q", out)
	}
	if len(f.said) != 1 {
		t.Fatalf("wrote %d messages, want one", len(f.said))
	}
}
