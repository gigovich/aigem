package bot

import (
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/tools"
)

func TestRolePresets(t *testing.T) {
	want := []string{"manager", "researcher", "architect", "developer", "tester"}
	got := RoleNames()
	if len(got) != len(want) {
		t.Fatalf("RoleNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RoleNames()[%d] = %q, want %q (order matters)", i, got[i], want[i])
		}
	}
	for _, n := range want {
		r, ok := RoleByName(n)
		if !ok {
			t.Fatalf("missing role %q", n)
		}
		if r.Prompt == "" {
			t.Errorf("role %q has empty prompt", n)
		}
		if len(r.Allow) == 0 {
			t.Errorf("role %q has empty allowlist", n)
		}
	}
}

// TestEveryRoleHasFullTooling locks in the policy that every bot may use the
// whole aigem toolset; roles differ only by prompt. bash + open_url are what
// let a bot reach an external API (e.g. plane) with an auth token.
func TestEveryRoleHasFullTooling(t *testing.T) {
	for _, r := range Roles() {
		have := make(map[string]bool, len(r.Allow))
		for _, tool := range r.Allow {
			have[tool] = true
		}
		for _, want := range full {
			if !have[want] {
				t.Errorf("role %q must allow %q (full toolset)", r.Name, want)
			}
		}
	}
}

// TestShippedPromptsAreDeploymentNeutral guards a real regression: the developer role once
// told every aigem user's bot to "escalate to Lisa", a bot name from one private deployment,
// and the tester role hardcoded that deployment's credentials path. Shipped prompt text is
// read by strangers on unrelated projects, so it must name no specific bot, host, org, or
// vendor - only roles and conventions.
func TestShippedPromptsAreDeploymentNeutral(t *testing.T) {
	corpus := ""
	for _, r := range Roles() {
		corpus += BaseSystemFor(r) + "\n" + r.Prompt + "\n" + operatingProtocol("botname", r)
	}
	// Tool descriptions and schemas are shipped to the model too, and are just as capable of
	// naming one deployment's bots: the handoff tool used to say `to (the teammate's name,
	// e.g. "jane")`, which no other user has.
	for _, tl := range []tools.Tool{
		NewHandoffTool(nil, nil, nil),
		NewPostMessageTool(nil, nil, nil),
		NewReadChatTool(nil, nil),
		NewTeamStatusTool("botname", NewFleet()),
		NewMemoryTool(NewStore(t.TempDir())),
		NewScheduleTool(nil),
		NewSaveSkillTool(t.TempDir()),
		NewDeleteSkillTool(t.TempDir()),
	} {
		corpus += "\n" + tl.Description() + "\n" + string(tl.Schema())
	}
	// Text the runtime injects at turn start, and the built-in jobs' prompts, are shipped to the
	// model just as much as the role prompts are.
	corpus += "\n" + workHeartbeatPrompt + "\n" + memoryReviewPrompt + "\n" + threadUpdatePreamble +
		"\n" + transientResumeInput + "\n" + budgetResumeInput("test")
	// The built-in system skills ship their listing and body to the model too.
	sys, _ := SystemSkills()
	for _, s := range sys.List() {
		corpus += "\n" + s.Listing() + "\n" + s.Body()
	}
	lower := strings.ToLower(corpus)
	for _, banned := range []string{
		"lisa", "amiran", "jane", "kate", "demetre", // deployment bot names
		"gitea", "mattermost", "doaml", "devinlab", "laban", "oxoauth", // hosts/orgs/vendors
		"gigovich", "#tasks", // deployment account and channel names
		"~/.config/keys", "~/work", // deployment filesystem layout
	} {
		if strings.Contains(lower, banned) {
			t.Errorf("shipped prompt text leaks deployment-specific %q; describe the role or "+
				"convention generically instead", banned)
		}
	}
}

// TestRolePromptsCarryExtractedLessons locks in the per-role lessons mined from the bots'
// memory, so a later reword cannot quietly drop one. Anchors are matched whitespace-normalized
// (see flat) because the prompts are hard-wrapped raw literals.
func TestRolePromptsCarryExtractedLessons(t *testing.T) {
	want := map[string][]string{
		"manager": {
			"Route by what the work needs",
			"never batch unrelated, risky, or mutually blocking changes",
			// A fully blocked board is exactly the state that stalled the team: the manager
			// stayed silent instead of asking the architect what could be unblocked.
			"ask the architect, by @mention, which of the blocked tickets can be unblocked",
			"Ask once per episode",
			"A board that is blocked, asked about, and recorded is not silence",
			// Progress was judged by "did he answer my check-in", which a stalled bot passes.
			"Judge progress by what moved, not by what you were told",
		},
		"researcher": {"hand that unfinished check to whoever can close it"},
		"architect": {
			"State the boundaries of a design",
			"Do not re-open a decision you already recorded",
		},
		"developer": {
			"A push is not a result",
			"Do not close your own work until someone else has independently verified it",
			"remove its check job",
			// One human-blocked ticket used to silence the developer entirely: it removed its
			// only wake-up and never looked at the rest of its assigned work.
			"Being blocked on one ticket does not make you idle",
		},
		"tester": {
			"You are the independent check",
			"add the missing test",
		},
	}
	for name, anchors := range want {
		r, ok := RoleByName(name)
		if !ok {
			t.Fatalf("role %q not found", name)
		}
		got := flat(r.Prompt)
		for _, a := range anchors {
			if !strings.Contains(got, a) {
				t.Errorf("role %q prompt lost the extracted lesson %q", name, a)
			}
		}
	}
}

// TestDeveloperCloseRuleHasSoloEscapeHatch guards a deadlock: "do not close your own work"
// plus "a ticket awaiting QA is still yours" plus a removed continuation job would strand a
// single-bot deployment that has no second agent to produce a verdict.
func TestDeveloperCloseRuleHasSoloEscapeHatch(t *testing.T) {
	r, _ := RoleByName("developer")
	if !strings.Contains(flat(r.Prompt), "no one else who can verify") {
		t.Error("developer close rule must have an escape hatch for a solo deployment")
	}
}

func TestDeveloperAllowsEdit(t *testing.T) {
	r, ok := RoleByName("developer")
	if !ok {
		t.Fatal("developer role not found")
	}
	has := func(name string) bool {
		for _, x := range r.Allow {
			if x == name {
				return true
			}
		}
		return false
	}
	if !has("write_file") || !has("bash") || !has("edit_file") {
		t.Fatalf("developer allowlist incomplete: %v", r.Allow)
	}
}

func TestEveryRoleHasMemory(t *testing.T) {
	for _, r := range Roles() {
		found := false
		for _, tool := range r.Allow {
			if tool == "memory" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("role %q must allow the memory tool", r.Name)
		}
	}
}

func TestEveryRoleHasCronTools(t *testing.T) {
	need := []string{"schedule", "post_message"}
	for _, r := range Roles() {
		for _, want := range need {
			found := false
			for _, tool := range r.Allow {
				if tool == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("role %q must allow %q", r.Name, want)
			}
		}
	}
}

func TestEveryRoleHasSkillTools(t *testing.T) {
	need := []string{"save_skill", "delete_skill", "skill"}
	for _, r := range Roles() {
		for _, want := range need {
			found := false
			for _, tool := range r.Allow {
				if tool == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("role %q must allow %q", r.Name, want)
			}
		}
	}
}

// TestNoRoleTellsItselfToStopWakingUp guards the deadlock that stopped the whole team: the
// developer prompt used to say to keep NO check armed while waiting on a human, which - combined
// with a removed watchdog - left the bot with no path back to work at all.
func TestNoRoleTellsItselfToStopWakingUp(t *testing.T) {
	banned := []string{
		"keep NO check armed",
		"not even a slow one",
	}
	for _, r := range Roles() {
		got := flat(r.Prompt)
		for _, b := range banned {
			if strings.Contains(got, b) {
				t.Errorf("role %q tells itself to leave no wake-up armed (%q)", r.Name, b)
			}
		}
	}
}

// TestDeveloperWatchdogGuidanceIsPurposeKeyed keeps two lessons from cancelling each other out: a
// watchdog may now be shorter than the whole turn budget (the busy gate makes that safe), but it
// must still never become a minute-scale continuation of the bot's own work, which is the original
// fragmentation incident.
func TestDeveloperWatchdogGuidanceIsPurposeKeyed(t *testing.T) {
	r, ok := RoleByName("developer")
	if !ok {
		t.Fatal("no developer role")
	}
	got := flat(r.Prompt)
	if !strings.Contains(got, "ten to twenty minutes, not two") {
		t.Error("developer prompt should aim a watchdog at when the external thing finishes")
	}
	// The original incident: minute-scale continuations turned one ticket into hundreds of log
	// lines. A later reword briefly reinstated "a few minutes out" as the usual shape.
	if !strings.Contains(got, "never on a minute scale") {
		t.Error("developer prompt must forbid minute-scale continuation jobs")
	}
	if !strings.Contains(got, "waits for that turn to finish") {
		t.Error("developer prompt should say a due job waits for a running turn instead of doubling up")
	}
	// The gate has a ceiling, so the prompt must not promise overlap is impossible.
	if strings.Contains(got, "so it can never double up on you") {
		t.Error("developer prompt overstates the busy gate; it has a ceiling and chat can still arrive")
	}
}
