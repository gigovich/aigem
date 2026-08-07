# Delegation evals

The unit tests prove the `task` tool *works*: a subagent runs, its tools are scoped, two calls in
one response run concurrently. They say nothing about whether the model *chooses* to delegate when
it should - that lives in the tool description and the system prompt, and it is a property of the
prompt, not of the code.

This harness measures that choice.

```sh
make build
go run ./evals/runner -model <model-ref> -n 3
```

Each scenario runs `aigem -p` against a throwaway copy of a fixture workspace, with
`--trace-json` recording the run. The trace is read back and checked against what the scenario
expects. Sampling is noisy, so `-n` repeats every scenario and the report is rates.

## What it measures

| Metric | Question |
| --- | --- |
| delegation recall | Did it delegate on tasks that call for a subagent? |
| delegation precision | Did it stay in the main agent on tasks that do not? |
| agent-type accuracy | Did the delegation reach an agent that can do the job? |
| parallel compliance | Did N subagents run **together**, not one after another? |
| self-containedness | Do delegated prompts stand alone, or lean on unseen conversation? |
| tool rounds / peak tokens | What did delegating cost, and what did it save? |

Every scenario also asserts that the work actually happened, and this is not optional - a scenario
without an outcome assertion is rejected at load. Otherwise "delegation precision" is a score a
model maximizes by doing less: a run that reads nothing and answers "I don't know" has, technically,
not over-delegated. A run that skips the work earns no credit in any rate and is counted separately
in the report.

Precision matters as much as recall. A model that delegates everything burns a full extra context
per trivial question, and a single "pass rate" would hide it - which is why every scenario declares
`delegate: required`, `forbidden`, or `optional` rather than just "should use subagents".

Size is what separates the two, so the fixtures differ in size on purpose. `notes` is a hundred
lines, where reading a file directly beats paying for a second context; `services` is three
independent packages of several hundred lines. A scenario that demanded a subagent for a
three-file project would be teaching the habit this suite exists to measure.

Even `services` is not large enough to make delegation pay by itself. Measured over 16 runs of
"explore each of the three services", the runs that delegated peaked at 12.4k tokens against 10.5k
for the runs that just read the code: each subagent's summary is nearly as long as the package
behind it. So only scenarios where the user **asks** for parallel work require delegating; the
rest score how it is done. Expectations here are calibrated against measurements, and any that
turn out to punish the better answer get changed - two already have.

Parallelism is the one thing the per-call events cannot show: three `task` calls in three
consecutive responses look identical to three in one, unless you record which calls the model
batched together. That is what `tool_batch` in the trace is for.

Self-containedness is a keyword heuristic (phrases like "as mentioned above" in a prompt handed to
an agent that cannot see the conversation). It is **reported, never failed on** - treat it as a
pointer to a transcript worth reading, not as a score.

## Scenarios

`scenarios.json` is a JSON array. Every entry carries a `why`, because an expectation nobody can
justify a year later is noise rather than a test.

```json
{
  "name": "parallel-two-services",
  "fixture": "services",
  "prompt": "Review the public HTTP API of services/alpha and services/beta. ...",
  "why": "Explicit 'in parallel' plus two named targets, each a few hundred lines.",
  "expect": { "delegate": "required", "min_parallel": 2 }
}
```

`name` also names the run's workspace directory, and `fixture` is a directory under `testdata/`,
so neither may contain a path separator. Agent names are checked against the subagents the `aigem`
binary will actually offer, built-ins plus `~/.config/aigem/agents/*.md`.

| `expect` field | Meaning |
| --- | --- |
| `delegate` | `required`, `forbidden`, or `optional` (default) |
| `agents` | at least one delegation must reach one of these; extras are reported, not failed |
| `min_parallel` | subagents that must run together from one response; needs `delegate: required` |
| `batched` | weaker: **if** the run delegates twice or more, the calls must go out together |
| `max_tasks` | cap on `task` calls, to catch a job sharded into a swarm |
| `answer_contains` | substrings the final answer must have, matched case-insensitively |
| `file_contains` / `file_absent` | path to text that must be present in / gone from the workspace |
| `unchanged` / `changed` | the run must leave the workspace alone, or must have edited it |

At least one of the last three rows is required.

Use `min_parallel` only where delegating is the right call regardless - typically because the user
asked for parallel work. Where that is a judgment call, `batched` scores how the run delegates
without ruling on whether it should have, and contributes to the rate only once it has delegated
twice.

Two counts are kept apart. `task_calls` is what the model issued; `delegations` is how many
subagents actually started. A call rejected before it runs - an unknown `agent_type`, an empty
prompt - was still the decision to delegate, so precision and `max_tasks` count it, while
`min_parallel` and recall need a subagent to have really started.

Fixtures live in `testdata/` so the Go tool ignores them - they are sample projects for the model
to work on, not packages of this module. Each run gets its own copy, so an edit in one run cannot
change what the next one sees.

## Flags

| Flag | Default | Notes |
| --- | --- | --- |
| `-bin` | `bin/aigem` | build it first with `make build` |
| `-scenarios` | `evals/scenarios.json` | repo-relative, so run from the repo root |
| `-fixtures` | `evals/testdata` | likewise |
| `-n` | `3` | runs per scenario |
| `-filter` | | substring match on the scenario name |
| `-model`, `-url` | | passed through to `aigem` |
| `-temp` | `0.3` | passed through to `aigem` |
| `-capability-profile` | `workspace-write` | see the note below |
| `-jobs` | `1` | raise only if the provider tolerates concurrent runs |
| `-timeout` | `5m` | per run |
| `-json` | | write the per-run results for diffing between prompt revisions |
| `-keep` | | keep the temp workspaces and traces to read a transcript |

`make evals` wraps the common case; extra flags go through `EVAL_ARGS`. The runner exits 1 when
scenarios fail and 2 when the harness itself broke - a missing binary, an unreadable fixture, a run
that produced no trace - so a dead backend does not read as a delegation regression.

Runs that a runaway budget cut short are reported but kept out of every rate. They end with a
plausible answer and no error, and the behavior after the cut is simply missing.

### About the profile

`workspace-write` withholds the shell, which keeps a run from executing whatever the model decides
to run on the fixture. It does **not** make a run hermetic: the profile still allows `web_search`,
`open_url`, and `browser_action`, and `-y` auto-approves them, so a run can reach the network.
File writes are confined to the run's own workspace copy.

The tradeoff is that `code-writer`, `simplifier`, and `reviewer` are told to run formatters,
linters, and tests, and without `bash` they cannot. That does not affect whether the model picks
the right agent, which is what the suite scores, but it does mean those subagents cannot finish
their instructions. Pass `-capability-profile shell` when you want them to; each fixture carries a
`go.mod` so `go test ./...` works inside the copied workspace.

## Reading the result

The numbers are only comparable against themselves: same model, same scenarios, same profile.
Record a baseline before changing a prompt, change one thing, re-run with the same `-n`, and
compare. A shift of one run out of three is noise.

Confounds the harness cannot remove:

- A custom `~/.config/aigem/SYSTEM.md` **replaces** the built-in base prompt, so the surrounding
  instructions being scored are yours. The delegation block is appended to whichever base is in
  effect and is present either way. The report says which case it is in its header.
- Custom subagents in `~/.config/aigem/agents/*.md` override the built-in definitions by name.
  Nothing warns about this, and it lands directly on agent-type accuracy.
- User-level skills, hooks, and MCP servers are live during a run, exactly as they are in normal
  use. That is realistic, not neutral.

The trace files hold the prompt, the final answer, and tool arguments and results (clipped to
400 bytes each). They are written 0600, and `-keep` is what leaves them on disk.
