package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runHandoff(t *testing.T, f *fakeWriter, local *LocalDelivery, args string) (string, error) {
	t.Helper()
	return NewHandoffTool(f, local, "amiran").Run(context.Background(), json.RawMessage(args))
}

// With no thread a handoff is new work, and the operator is in it from the
// start so no bot-to-bot conversation happens out of sight.
func TestHandoffWithNoThreadOpensOneWithTheOperatorInIt(t *testing.T) {
	f := newFakeWriter()

	out, err := runHandoff(t, f, nil, `{"to":"demetre","summary":"QA the settings pane"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %d threads", len(f.opened))
	}
	got := f.opened[0].Participants
	for _, want := range []string{"bot:demetre", "bot:amiran", "human:operator"} {
		if !contains(got, want) {
			t.Fatalf("participants = %v, want %s among them", got, want)
		}
	}
	if !strings.Contains(out, "handed off to demetre") {
		t.Fatalf("result = %q", out)
	}
	said := f.lastSaid()
	if !strings.Contains(said.Text, "**Handoff**") || !strings.Contains(said.Text, "QA the settings pane") {
		t.Fatalf("the message does not read as a handoff: %q", said.Text)
	}
}

// Inside an existing thread the teammate is pulled into it rather than being
// given a second place to talk about the same work.
func TestHandoffInAThreadJoinsTheTeammateToIt(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")

	if _, err := runHandoff(t, f, nil,
		`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`); err != nil {
		t.Fatal(err)
	}
	if len(f.opened) != 0 {
		t.Fatal("a thread was opened when one was named")
	}
	if len(f.joined) != 1 || f.joined[0].Actor != "bot:demetre" ||
		f.joined[0].Thread != "t_0102030405060708" {
		t.Fatalf("joined = %+v", f.joined)
	}
	if f.lastSaid().Thread != "t_0102030405060708" {
		t.Fatalf("the handoff landed in %q", f.lastSaid().Thread)
	}
}

// The teammate is named on the message, which is what the UI draws and what the
// classifier reads.
func TestHandoffMentionsTheTeammate(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	if _, err := runHandoff(t, f, nil,
		`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`); err != nil {
		t.Fatal(err)
	}
	if got := f.lastSaid().Opts.Mentions; len(got) != 1 || got[0] != "bot:demetre" {
		t.Fatalf("mentions = %v", got)
	}
}

func TestHandoffCarriesTheTicket(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	if _, err := runHandoff(t, f, nil,
		`{"to":"demetre","summary":"QA it","ticket":"#42","thread":"t_0102030405060708"}`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastSaid().Text, "[#42]") {
		t.Fatalf("the ticket is missing: %q", f.lastSaid().Text)
	}
}

func TestHandoffNeedsATeammateAndASummary(t *testing.T) {
	f := newFakeWriter()
	for _, args := range []string{
		`{"summary":"QA it"}`,
		`{"to":"demetre"}`,
		`{"to":"  ","summary":"QA it"}`,
		`{"to":"demetre","summary":"   "}`,
	} {
		if _, err := runHandoff(t, f, nil, args); err == nil {
			t.Fatalf("%s was accepted", args)
		}
	}
	if len(f.said) != 0 || len(f.opened) != 0 {
		t.Fatal("a refused handoff wrote something anyway")
	}
}

// A handoff to a name nothing answers to notifies nobody, which is the one
// thing a handoff must not do quietly.
func TestHandoffRefusesAnUnknownTeammate(t *testing.T) {
	f := newFakeWriter()

	_, err := runHandoff(t, f, nil, `{"to":"ghost","summary":"QA it"}`)
	if err == nil {
		t.Fatal("a handoff to nobody reported success")
	}
	if !strings.Contains(err.Error(), "team_status") {
		t.Fatalf("error does not say how to find who there is: %v", err)
	}
	if len(f.opened) != 0 || len(f.said) != 0 {
		t.Fatal("the handoff was written anyway")
	}
}

// Knowing the teammate is already working is what stops a second ping, and it
// has to be read before the delivery that would make them busy.
func TestHandoffReportsABusyTeammate(t *testing.T) {
	f := newFakeWriter("t_0102030405060708")
	fleet := NewFleet()
	rt := NewRuntime(&fakeTransport{in: make(chan Inbound)}, nil, 1)
	fleet.Register(Member{Name: "demetre", Actor: "bot:demetre", Runtime: rt,
		Participation: allParticipation{}})
	// Hold the turn for the length of the test: the note exists to stop a second
	// ping while the first is still being worked on.
	defer rt.EnterTurn()()

	local := &LocalDelivery{Self: "amiran", SelfActor: "bot:amiran", Fleet: fleet}
	out, err := runHandoff(t, f, local,
		`{"to":"demetre","summary":"QA it","thread":"t_0102030405060708"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mid-turn") {
		t.Fatalf("result does not say the teammate is busy: %q", out)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
