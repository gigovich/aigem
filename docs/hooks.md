# Hooks

aigem runs Claude-compatible [hooks](https://code.claude.com/docs/en/hooks.md):
shell commands fired at lifecycle points to observe, block, modify, or enrich a
turn.

## Where they are defined

Definitions are read from `settings.json` files and merged additively - every
matching hook runs - in this load order:

1. `<project>/.aigem/settings.json`
2. `~/.config/aigem/settings.json`
3. `<project>/.claude/settings.json` and `.claude/settings.local.json`
4. `~/.claude/settings.json`

Set `"disableAllHooks": true` in any of them to turn hooks off entirely.

## Events

| Event              | When it fires    | What it can do                                                     |
| ------------------ | ---------------- | ------------------------------------------------------------------ |
| `PreToolUse`       | before a tool     | deny, force/skip confirmation, rewrite the tool input, add context |
| `PostToolUse`      | after a tool      | replace the output, add context                                    |
| `UserPromptSubmit` | before a turn     | block, or inject context                                           |
| `Stop`             | model finishes    | block to force more work (capped per turn)                         |
| `SubagentStop`     | subagent finishes | same, for a subagent                                               |
| `SessionStart`     | session opens     | `additionalContext` enriches the system prompt; may set `sessionTitle` |
| `SessionEnd`       | session closes    | best-effort cleanup                                                |
| `Notification`     | on a confirmation | best-effort notice                                                 |
| `PreCompact`       | before compaction | observe an impending summarization                                 |

A skill's `hooks:` frontmatter registers hooks active only while that skill is
active. Only the `command` handler type is supported.

## The contract

Each hook command receives the event JSON on stdin (`hook_event_name`,
`tool_name`, `tool_input`, `tool_response`, `prompt`, ...), runs in the project
directory with `CLAUDE_PROJECT_DIR` set, and steers the agent by exit code and
stdout JSON:

- **exit 2** blocks. stderr is the reason fed back to the model.
- **exit 0** may print `{"decision":"block","reason":...}`, `"continue":false`, or
  a `hookSpecificOutput` with `permissionDecision` (`allow`/`deny`/`ask`),
  `updatedInput`, `updatedToolOutput`, or `additionalContext`.
- **any other exit code** is a non-blocking notice.

`tool_input` is the raw tool-argument JSON. `tool_response` is the tool's textual
result - a plain string in aigem, not a structured object.

## Trust

Hooks are author-trusted: a `command` runs an arbitrary shell with your full user
privileges.

Global hooks (`~/.config/aigem`, `~/.claude`) always run. **Project-local hooks**
(`<project>/.aigem` and `.claude`) run only after you approve the current hook
configuration: the TUI asks at startup (press `y`), and non-interactive runs
(`-p`, `--repl`) skip them unless you pass `--trust-project-hooks`. The approval is
independent of skills and MCP, and is invalidated when the effective project hook
configuration changes.

## Troubleshooting

Malformed hook entries - a bad matcher regexp, an unsupported `type`, an empty
`command` - are reported as warnings at startup and skipped.

Set `AIGEM_HOOKS_DEBUG=1` to log each hook's event, command, exit code, and output
to stderr.

## Example

A `<project>/.aigem/settings.json` that blocks `bash` calls touching `.env`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "bash",
        "hooks": [
          {
            "type": "command",
            "command": "grep -q '\\.env' <<<\"$(jq -r .tool_input.command)\" && { echo 'no .env access' >&2; exit 2; } || exit 0"
          }
        ]
      }
    ]
  }
}
```
