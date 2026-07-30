# Skills

aigem discovers Claude-compatible
[Agent Skills](https://code.claude.com/docs/en/skills.md). A skill is a directory
with a `SKILL.md`: YAML frontmatter plus markdown instructions.

## Discovery

Skills are found, in priority order, in:

1. `<project>/.skills`
2. `~/.config/aigem/skills`
3. `<project>/.claude/skills`
4. `~/.claude/skills`

aigem's `.skills` shadows `.claude/skills`. Project locations include parent
directories up to the git root, and nested directories.

Only each skill's name and description goes into context up front; the body loads
when the skill is invoked. The model invokes one with the `skill` tool. You can
run any of them with `/skill:<name> [args]` (shown as a pink skill line), and
`/skills` opens a scrollable browser - Enter for detail, Enter again to run.

## Trust

Project-local skills are **not discovered until their current definitions are
approved**, because rendering a skill can execute dynamic shell or hooks and
mutate turn tool policy through `allowed-tools`.

The TUI is the only front-end that asks: at startup it lists the withheld skills.
Press `y` to approve and load them into the running session - no restart needed.
`Ctrl+C` quits; any other key skips. Skipping records nothing, so relaunching asks
again.

`-p` and `--repl` print a warning naming the withheld skills and skip them unless
you pass `--trust-project-skills`.

Changing any project skill definition invalidates that approval, and the prompt
returns saying so. See [the security model](security.md) for how approvals are
fingerprinted.

## Frontmatter

| Key                        | Meaning                                                            |
| -------------------------- | ------------------------------------------------------------------ |
| `name`                     | the skill's identifier                                             |
| `description`              | shown to the model, and in the browser                             |
| `when_to_use`              | extra guidance on applicability                                    |
| `argument-hint`            | hint text for arguments                                            |
| `arguments`                | named arguments, usable as `$name` in the body                     |
| `allowed-tools`            | pre-approve tools for the turn                                     |
| `disallowed-tools`         | block tools for the turn                                           |
| `disable-model-invocation` | the model cannot invoke it; you still can                          |
| `user-invocable`           | whether `/skill:<name>` works                                      |
| `context: fork`            | run the skill in an isolated subagent                              |
| `paths`                    | keep the skill out of the listing until a tool touches a match     |

Claude tool names map onto aigem's. **Project-local skills cannot pre-approve
`bash`**, regardless of `allowed-tools`.

## Body substitutions

Bodies support `$ARGUMENTS`, positional `$0..$n`, named `$name`,
`${CLAUDE_SKILL_DIR}`, and dynamic ``` !`cmd` ``` injection.
