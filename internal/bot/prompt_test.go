package bot

import (
	"strings"
	"testing"
)

func TestComposeSystemLayers(t *testing.T) {
	role, _ := RoleByName("developer")
	c := Config{Name: "amiran", Role: "developer"}
	got := ComposeSystem(c, role,
		"MEMORY-INDEX-BLOCK", "SKILLS-CATALOG-BLOCK", "DATE-AND-PROJECT")
	for _, want := range []string{
		"You are aigem", "autonomous aigem bot", "amiran",
		"Your role is developer", "MEMORY-INDEX-BLOCK", "SKILLS-CATALOG-BLOCK", "DATE-AND-PROJECT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing %q", want)
		}
	}
	// Ordering: base < protocol < role < memory < skills < extra.
	order := []string{
		"You are aigem", "autonomous aigem bot", "Your role is developer",
		"MEMORY-INDEX-BLOCK", "SKILLS-CATALOG-BLOCK", "DATE-AND-PROJECT",
	}
	for i := 1; i < len(order); i++ {
		if strings.Index(got, order[i-1]) > strings.Index(got, order[i]) {
			t.Errorf("layer %q must precede %q", order[i-1], order[i])
		}
	}
	// Every role composes cleanly: base present, no blank-line gaps.
	for _, r := range Roles() {
		out := ComposeSystem(Config{Name: "amiran", Role: r.Name}, r, "", "", "")
		if !strings.HasPrefix(out, "You are aigem") {
			t.Errorf("%s prompt must start with the base layer", r.Name)
		}
		if strings.Contains(out, "\n\n\n") {
			t.Errorf("%s prompt has a stray blank gap", r.Name)
		}
	}
}

func TestBaseSystemOnlyReferencesRealBotTools(t *testing.T) {
	// The bot registry has no todo_write or task/sub-agent tool, so the base must not
	// command their use - a mandate the bot cannot satisfy.
	for _, r := range Roles() {
		base := BaseSystemFor(r)
		for _, banned := range []string{"todo_write", "task tool", "sub-agent", "subagent", "delegate"} {
			if strings.Contains(strings.ToLower(base), banned) {
				t.Errorf("%s base must not reference %q: bots have no such tool", r.Name, banned)
			}
		}
	}
	base := BaseSystemFor(mustRole(t, "developer"))
	for _, want := range []string{"read_file", "edit_file", "write_file", "grep", "bash"} {
		if !strings.Contains(flat(base), want) {
			t.Errorf("developer base should mention the real tool %q", want)
		}
	}
}

func TestBaseSystemIsRoleConditional(t *testing.T) {
	for _, name := range []string{"manager", "researcher"} {
		base := BaseSystemFor(mustRole(t, name))
		for _, banned := range []string{
			"Editing files:", "gofmt", "edit_file", "write_file",
			"After a change, run the language's formatter",
		} {
			if strings.Contains(flat(base), flat(banned)) {
				t.Errorf("%s base must not carry editing mechanics, found %q", name, banned)
			}
		}
	}
	arch := BaseSystemFor(mustRole(t, "architect"))
	for _, want := range []string{"Writing documents:", "write_file", "edit_file"} {
		if !strings.Contains(arch, want) {
			t.Errorf("architect base must carry document-writing mechanics, missing %q", want)
		}
	}
	for _, banned := range []string{"Editing files:", "gofmt"} {
		if strings.Contains(arch, banned) {
			t.Errorf("architect base must not carry code-editing mechanics, found %q", banned)
		}
	}
	for _, name := range []string{"developer", "tester"} {
		base := BaseSystemFor(mustRole(t, name))
		for _, want := range []string{"Editing files:", "gofmt"} {
			if !strings.Contains(base, want) {
				t.Errorf("%s base must carry full editing mechanics, missing %q", name, want)
			}
		}
	}
	// The honesty rules bind every role: a researcher fabricating a URL is exactly the
	// failure they prevent.
	for _, r := range Roles() {
		base := flat(BaseSystemFor(r))
		for _, want := range []string{
			"Never ship something that only looks finished", "Do not invent external identifiers",
		} {
			if !strings.Contains(base, want) {
				t.Errorf("%s base must keep the honesty rules, missing %q", r.Name, want)
			}
		}
	}
}

func TestOperatingProtocolHasMemorySection(t *testing.T) {
	role, _ := RoleByName("manager")
	got := operatingProtocol("amiran", role)
	if !strings.Contains(got, "## Memory") {
		t.Error("operating protocol must include a Memory section")
	}
}

func TestComposeSystemOmitsEmptyLayers(t *testing.T) {
	role, _ := RoleByName("developer")
	c := Config{Name: "amiran", Role: "developer"}
	got := ComposeSystem(c, role, "", "", "EXTRA")
	if !strings.Contains(got, "EXTRA") {
		t.Error("extra must still be present when memory and skills are empty")
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("empty memory/skills layers left a stray blank gap:\n%q", got)
	}
}

func TestOperatingProtocolHasSchedulingSection(t *testing.T) {
	role, _ := RoleByName("manager")
	got := operatingProtocol("amiran", role)
	if !strings.Contains(got, "## Scheduling") {
		t.Error("operating protocol must include a Scheduling section")
	}
	if !strings.Contains(got, "post_message") {
		t.Error("operating protocol should mention post_message")
	}
}

func TestOperatingProtocolHasMultiBotSection(t *testing.T) {
	role, _ := RoleByName("developer")
	got := operatingProtocol("amiran", role)
	if !strings.Contains(got, "## Working alongside other bots") {
		t.Error("operating protocol must explain how to behave alongside other bots")
	}
	if !strings.Contains(got, "@here") {
		t.Error("operating protocol should address broadcast mentions like @here")
	}
}

func TestOperatingProtocolWarnsAgainstMentionLoops(t *testing.T) {
	role, _ := RoleByName("manager")
	got := operatingProtocol("lisa", role)
	if !strings.Contains(got, "acknowledgement loop") {
		t.Error("operating protocol must warn against bot-to-bot acknowledgement loops")
	}
	if !strings.Contains(got, "it wakes that participant") {
		t.Error("operating protocol must explain that an @mention wakes the mentioned bot")
	}
}

func TestOperatingProtocolTeachesNoReply(t *testing.T) {
	role, _ := RoleByName("manager")
	got := operatingProtocol("lisa", role)
	if !strings.Contains(got, "NO_REPLY") {
		t.Error("operating protocol must teach the NO_REPLY silence sentinel")
	}
	if !strings.Contains(got, "acknowledge an acknowledgement") {
		t.Error("operating protocol must forbid acknowledging acknowledgements")
	}
	if !strings.Contains(got, "post only when something actually changed") {
		t.Error("scheduling section must gate recurring status posts on a change")
	}
	if !strings.Contains(got, "re-check the live state") {
		t.Error("working discipline must require re-checking live state before status replies")
	}
}

// TestOperatingProtocolTeachesThreadDiscipline locks in the most-replicated lesson in the
// bots' memory: every bot independently recorded "one work item, one thread" after the
// channel became unreadable from update-per-root-post.
func TestOperatingProtocolTeachesThreadDiscipline(t *testing.T) {
	got := flat(operatingProtocol("amiran", mustRole(t, "developer")))
	for _, want := range []string{"One work item, one thread", "reply there"} {
		if !strings.Contains(got, want) {
			t.Errorf("protocol must teach thread discipline, missing %q", want)
		}
	}
}

// TestOperatingProtocolRequiresReplyWhenAddressed guards against over-applied silence: logs
// showed the manager bot answering 119 of its 158 inbound direct messages (75%) with msg=silent.
func TestOperatingProtocolRequiresReplyWhenAddressed(t *testing.T) {
	got := flat(operatingProtocol("lisa", mustRole(t, "manager")))
	if !strings.Contains(got, "always gets a reply") {
		t.Error("protocol must require answering a DM or direct mention")
	}
	if !strings.Contains(got, "the item's tracker history") {
		t.Error("protocol must check the tracker, not just the thread, before re-posting")
	}
}

func TestOperatingProtocolTeachesIdentityAndSecrets(t *testing.T) {
	got := flat(operatingProtocol("jane", mustRole(t, "tester")))
	for _, want := range []string{"Act as yourself in every external system", "never the user's"} {
		if !strings.Contains(got, want) {
			t.Errorf("protocol must teach acting under own identity, missing %q", want)
		}
	}
	if !strings.Contains(got, "never output") {
		t.Error("protocol must forbid echoing credentials")
	}
}

// TestOperatingProtocolBoundsOutreach covers the ping-once rule and its single bounded
// repair, learned separately by three bots.
func TestOperatingProtocolBoundsOutreach(t *testing.T) {
	got := flat(operatingProtocol("lisa", mustRole(t, "manager")))
	if !strings.Contains(got, "Ping exactly once") {
		t.Error("protocol must bound outreach to a single ping")
	}
	if !strings.Contains(got, "repair it ONCE") {
		t.Error("protocol must allow exactly one stalled-handoff repair before escalating")
	}
	if !strings.Contains(got, "Do not poll for something that will arrive as a message") {
		t.Error("scheduling must not poll for an event that wakes the bot anyway")
	}
}

func TestOperatingProtocolForbidsShadowTracking(t *testing.T) {
	got := flat(operatingProtocol("kate", mustRole(t, "architect")))
	if !strings.Contains(got, "Files hold deliverables, not the state of work") {
		t.Error("protocol must keep work-item state in the tracker, not local files")
	}
}

func TestOperatingProtocolForbidsPhantomState(t *testing.T) {
	got := flat(operatingProtocol("amiran", mustRole(t, "developer")))
	if !strings.Contains(got, "Do not act on state you only believe you created") {
		t.Error("protocol must require confirming a schedule/memory handle exists")
	}
}

// TestBaseSystemTeachesToolDiscipline covers the two highest-volume tool failures in the logs:
// guessed paths (155 of 519 tool errors, 30%, with a "did you mean" hint offered 85 times) and
// blind retry of an identical failing call.
func TestBaseSystemTeachesToolDiscipline(t *testing.T) {
	for _, r := range Roles() {
		base := flat(BaseSystemFor(r))
		for _, want := range []string{
			"Never guess a path",
			"did you mean",
			"fails twice with the same error",
		} {
			if !strings.Contains(base, want) {
				t.Errorf("%s base must teach tool discipline, missing %q", r.Name, want)
			}
		}
	}
}

// TestBaseSystemForbidsFakeWork covers the largest ticket category in the closed-issue
// record: controls that render but do nothing, and hard-coded data presented as live.
func TestBaseSystemForbidsFakeWork(t *testing.T) {
	base := flat(BaseSystemFor(mustRole(t, "developer")))
	if !strings.Contains(base, "Never ship something that only looks finished") {
		t.Error("BaseSystem must forbid non-functional or faked UI and data")
	}
	if !strings.Contains(base, "Do not invent external identifiers") {
		t.Error("BaseSystem must forbid fabricating URLs, endpoints, and addresses")
	}
	if !strings.Contains(base, "Name what you did NOT verify") {
		t.Error("BaseSystem must require stating unrun checks")
	}
}

// flat collapses all runs of whitespace to single spaces so prompt assertions survive
// re-wrapping of the hard-wrapped raw string literals they anchor on.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustRole(t *testing.T, name string) Role {
	t.Helper()
	r, ok := RoleByName(name)
	if !ok {
		t.Fatalf("role %q not found", name)
	}
	return r
}

func TestComposeSystemIncludesPersona(t *testing.T) {
	role, _ := RoleByName("architect")
	c := Config{Name: "kate", Role: "architect", Persona: "female; feminine forms in Russian"}
	got := ComposeSystem(c, role, "", "", "")
	if !strings.Contains(got, "# Persona") || !strings.Contains(got, "feminine forms in Russian") {
		t.Error("persona from bot config must be composed into the system prompt")
	}
	noPersona := ComposeSystem(Config{Name: "kate", Role: "architect"}, role, "", "", "")
	if strings.Contains(noPersona, "# Persona") {
		t.Error("persona block must be omitted when unset")
	}
}

func TestOperatingProtocolHasSkillsSection(t *testing.T) {
	role, _ := RoleByName("developer")
	got := operatingProtocol("amiran", role)
	if !strings.Contains(got, "## Skills") {
		t.Error("operating protocol must include a Skills section")
	}
	if !strings.Contains(got, "save_skill") {
		t.Error("operating protocol should mention save_skill")
	}
}

func TestOperatingProtocolReferencesSystemSkills(t *testing.T) {
	role, _ := RoleByName("manager")
	got := flat(operatingProtocol("amiran", role))
	if len(SystemSkillNames()) == 0 {
		t.Fatal("no system skills embedded")
	}
	for _, name := range SystemSkillNames() {
		if !strings.Contains(got, name) {
			t.Errorf("operating protocol must mention the built-in skill %q", name)
		}
	}
}

func TestOperatingProtocolTeachesHoldsAndInterruptions(t *testing.T) {
	role, _ := RoleByName("developer")
	got := operatingProtocol("amiran", role)
	if !strings.Contains(got, "## Stopping, holds, and unanswered questions") {
		t.Fatal("operating protocol must cover stopping and holds")
	}
	// A hold has to outlive the message that carried it, or the next heartbeat
	// restarts the work it stopped.
	for _, want := range []string{
		"record the hold in the same turn",
		"Only the person who imposed a hold lifts it",
		"a handoff is a request, never a release",
		"An unanswered question stays unanswered",
		"handed to you in the middle of that turn",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("operating protocol should say %q", want)
		}
	}
}

func TestWorkHeartbeatRefusesHeldWork(t *testing.T) {
	for _, want := range []string{"hold nobody has explicitly", "never got answered"} {
		if !strings.Contains(workHeartbeatPrompt, want) {
			t.Errorf("the heartbeat prompt should say %q", want)
		}
	}
}
