# Configuration

## Where things live

aigem honors the XDG base directories.

| Path                                      | Contents                                            |
| ----------------------------------------- | --------------------------------------------------- |
| `~/.config/aigem/SYSTEM.md`               | system prompt override                              |
| `~/.config/aigem/settings.json`           | global hooks and MCP servers                        |
| `~/.config/aigem/models.json`             | your own providers and models                       |
| `~/.config/aigem/agents/*.md`             | custom subagents                                    |
| `~/.config/aigem/skills/`                 | global skills                                       |
| `~/.local/state/aigem/auth.json`          | credentials (`0600`)                                |
| `~/.local/state/aigem/sessions/`          | saved conversations                                 |
| `~/.local/state/aigem/path-grants.json`   | approved read paths outside a working directory     |
| `~/.local/state/aigem/project-trust.json` | approved project hooks, skills, and MCP targets     |
| `~/.local/state/aigem/local.json`         | local llama.cpp server settings                     |
| `~/.local/state/aigem/preferences.json`   | the model you last selected                         |
| `~/.local/state/aigem/input-history/`     | per-working-directory TUI input history (`0600`)    |
| `~/.local/state/aigem/search.json`        | web-search backend and its API key (`0600`)         |
| `~/.local/state/aigem/mcp-oauth/`         | MCP OAuth tokens, one file per server (`0600`)      |
| `~/.local/state/aigem/browser-profile/`   | the isolated Chrome profile for browser search      |

On macOS the config directory is `~/Library/Application Support/aigem`.

Project-local configuration lives in `<project>/.aigem/` and, for compatibility,
`<project>/.claude/`. Anything project-local that executes is
[trust-gated](security.md#project-trust).

### No longer used

aigem no longer reads or writes these paths, left over from the removed bot fleet
and web UI. It does not delete them automatically; remove them by hand if you
want the disk space back.

- `~/.config/aigem/bots/` (per-bot `bot.yaml`, `memory/`, `skills/`)
- `~/.config/aigem/fleet.json`
- `~/.local/state/aigem/chat/` (`chat.db`, `blobs/`, `vapid.json`)
- `~/.local/state/aigem/chat-cookies.json`
- `~/.local/state/aigem/chat.json`
- `~/.local/state/aigem/web-cookies.json`
- `~/.local/state/aigem/web.json`
- `~/.local/state/aigem/browser-profile/<botname>/` - only the per-bot
  subdirectories; the parent directory is still used by the interactive
  browser tool
- `~/.local/state/aigem/journal/<id>/blobs/`

## System prompt

The agent ships with a built-in prompt. To replace it entirely, create
`~/.config/aigem/SYSTEM.md`; its contents become the system prompt.

Replacing it does not leave the agent unable to use its tools. What a capability
is and when to reach for it - the subagents, the skill catalog, web search, MCP
servers, the project's own instruction files - is appended to whichever prompt
is in effect, because a tool the model can call but nothing explains is worse
than no tool at all. `SYSTEM.md` governs how the agent works, not what it has.

## Project conventions

At startup the harness discovers project instruction files and appends them to the
system prompt - for the main agent and every subagent - rather than relying on the
model to go find them:

They are injected in this order:

1. `AGENTS.md` / `CLAUDE.md` at the git root, walking up from `--cwd`. If no
   repository is found, at `--cwd` itself. If both exist as separate files, the
   most recently modified one wins; if one is a symlink to the other, it is
   loaded once.
2. `context.md` at the project root, injected in full.
3. `<cwd>/.claude/CLAUDE.md`, unless the root file already resolves to it.

These files are prompt-only text. They do not receive or confer executable trust.

## Environment variables

| Variable            | Effect                                                              |
| ------------------- | ------------------------------------------------------------------- |
| `OPENAI_API_KEY`    | authenticates `openai`. It overrides a stored openai *API-key* record; against a stored ChatGPT OAuth login it is used only for models outside the Codex allow-list |
| `XAI_API_KEY`       | authenticates `xai`. It **overrides** a stored xAI OAuth record  |
| `AIGEM_HOOKS_DEBUG` | any non-empty value logs every hook invocation to stderr            |
| `XDG_CONFIG_HOME`   | overrides the config directory                                      |
| `XDG_STATE_HOME`    | overrides the state directory                                       |
