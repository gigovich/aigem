# The TUI

## Keys

| Key                          | Action                                                    |
| ---------------------------- | --------------------------------------------------------- |
| `Enter`                      | send                                                      |
| `Shift+Enter` / `Alt+Enter`  | newline                                                   |
| `↑` / `↓`                    | recall previous / next input (single-line input only)     |
| Mouse wheel, `PgUp`, `PgDn`  | scroll the conversation                                   |
| `Shift+↑` / `Shift+↓`        | scroll by two lines                                       |
| `Shift+←` / `Shift+→`        | scroll a wide diff horizontally                           |
| `Esc`                        | interrupt the turn (cancels generation and running `bash`) |
| `Ctrl+O`                     | show/hide tool output (the `⤷` result preview)            |
| `Ctrl+V`                     | paste, including an image from the clipboard              |
| `Ctrl+T`                     | show/hide the todo panel                                  |
| `Shift+Tab`                  | toggle auto mode                                          |
| `Ctrl+C`                     | quit                                                      |

The view follows new output only while you are at the bottom. Scroll up to read
history and it stays put - the status bar shows `↑ N%` until you `PgDn` back down.

## Confirmations and auto mode

When a tool needs approval, a small box above the input asks. Use `←`/`→` to
select and `Enter` to confirm. What it offers depends on what is being approved:

| Situation                          | Options                                    | Remembered              |
| ---------------------------------- | ------------------------------------------ | ----------------------- |
| A confirm-gated tool               | `Once`, `Always`, `Forbid`                 | rest of the session      |
| Reading a path outside the workdir | `Once`, `Always (this folder)`, `Deny`     | `Always` persists to disk |
| Writing a path outside the workdir | `Once`, `Deny`                             | never                    |

`Always (this folder)` is written to `path-grants.json` and so survives across
sessions - see [the security model](security.md#the-filesystem-sandbox).

`Shift+Tab` toggles **auto mode**, shown as `⇧⇥ auto` in the status bar. In auto
mode every edit and safe command is approved automatically, so the agent runs
without interruption - but an irreversible action that cannot be reconstructed
from the code (`rm`, `git reset --hard`, `git clean`, `truncate`, `DROP TABLE`, ...)
still stops for an explicit confirmation.

## Slash commands

| Command             | What it does                                        |
| ------------------- | --------------------------------------------------- |
| `/model`            | fuzzy model picker; also `init`/`status`/`start`/`stop`/`reset` |
| `/login`, `/logout` | provider authentication                             |
| `/skills`           | scrollable skill browser                            |
| `/skill:<name>`     | run a skill directly                                |
| `/agents`           | subagent list, and the web-search config editor     |
| `/resume`           | pick a past session and continue it                 |
| `/new`              | start a fresh conversation                          |
| `/compact`          | summarize the conversation on demand                |
| `/artifacts`        | browse files the session produced                   |
| `/mcp`              | connected MCP servers                               |
| `/mcp__<server>__<prompt>` | run a prompt exposed by an MCP server        |

## Sessions

Conversations are saved as JSON under `~/.local/state/aigem/sessions` (honoring
`XDG_STATE_HOME`) after every turn. `/resume` picks a past session from a list and
continues it; resumed sessions always run on the current system prompt.

The status bar shows an authentication dot, the model ref, a context-usage gauge
(`ctx used/window %`) that turns peach past 50% and red past 80%, the ready or
working state, and a `^O tools shown` / `^O tools hidden` hint.

## Context compaction

Long sessions are kept inside the context window by a three-stage cascade that
escalates with pressure (used tokens / `--ctx-size`):

1. At `--evict-at-pct` (default 50%) old `read_file` and tool outputs are replaced
   with an `[output elided to save context - re-run <tool> to retrieve it]`
   placeholder. The tool call itself is
   kept, and the most recent `--compact-keep-tools` results plus the verbatim tail
   stay intact.
2. The same pass drops duplicate `read_file` outputs, keeping only the latest read
   of a path.
3. At `--compact-at-pct` (default 70%) older turns are summarized into a single
   `<summary>` message, preserving the system prompt, the original goal, and the
   most recent `--compact-keep-turns` messages verbatim.

`/compact [instructions]` summarizes on demand; optional instructions tell the
model what to preserve. aigem validates the returned `<summary>` structure before
replacing the conversation; if it is malformed, it emits a notice and retries once
with corrective guidance.

Before each summarization the full history is backed up with private `0600`
permissions to `~/.local/state/aigem/sessions/<id>.precompact-<n>.json`, and a
`PreCompact` hook fires. Disable the automatic path with `--compact-auto=false`.

## Runaway handling

Two guards keep the agent from spinning:

- **Live repetition detection** aborts a response as soon as the model emits the
  same line about six times in a row - a common "thinking" loop - then forces a
  final answer.
- **`--max-tokens`** caps each response, so unbounded generation cannot run
  forever. A truncated response shows a notice.

Interactive turns keep unbounded tool-loop behavior, because a human can press
`Esc` or `Ctrl+C`. Unattended `-p` and bot turns additionally enforce the
[turn budgets](security.md#turn-budgets), returning `Budget exhausted: ...` when a
loop exceeds them.

Each guard shows a `⚠` notice when it fires.

## Rendering and theme

Completed assistant answers are rendered as Markdown via
[glamour](https://github.com/charmbracelet/glamour). Live streaming text and `-p`
output stay plain.

The interface uses the [Catppuccin Mocha](https://catppuccin.com/palette) palette
on a solid `#181825` canvas - the mantle shade rather than the lighter `#1e1e2e`
base, so the whole frame reads dark and flat. A truecolor terminal is
recommended.
