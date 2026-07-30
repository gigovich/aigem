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
| `~/.config/aigem/bots/<name>/bot.yaml`    | one bot's definition                                |
| `~/.config/aigem/bots/<name>/memory/`     | that bot's persisted memory                         |
| `~/.local/state/aigem/auth.json`          | credentials (`0600`)                                |
| `~/.local/state/aigem/sessions/`          | saved conversations                                 |
| `~/.local/state/aigem/path-grants.json`   | approved read paths outside a working directory     |
| `~/.local/state/aigem/project-trust.json` | approved project hooks, skills, and MCP targets     |
| `~/.local/state/aigem/local.json`         | local llama.cpp server settings                     |

On macOS the config directory is `~/Library/Application Support/aigem`.

Project-local configuration lives in `<project>/.aigem/` and, for compatibility,
`<project>/.claude/`. Anything project-local that executes is
[trust-gated](security.md#project-trust).

## System prompt

The agent ships with a built-in prompt. To replace it entirely, create
`~/.config/aigem/SYSTEM.md`; its contents become the system prompt.

## Project conventions

At startup the harness discovers project instruction files and appends them to the
system prompt - for the main agent and every subagent - rather than relying on the
model to go find them:

- `AGENTS.md` / `CLAUDE.md` at the git root, walking up from `--cwd`. If no
  repository is found, at `--cwd` itself. If both exist as separate files, the
  most recently modified one wins; if one is a symlink to the other, it is loaded
  once.
- `<cwd>/.claude/CLAUDE.md`, unless the root file already resolves to it.
- `context.md` at the project root, which is injected in full.

These files are prompt-only text. They do not receive or confer executable trust.

## Environment variables

| Variable            | Effect                                                              |
| ------------------- | ------------------------------------------------------------------- |
| `OPENAI_API_KEY`    | authenticates `openai`. With a stored ChatGPT login it is used for models outside the Codex allow-list; with no stored login it is used for every openai model |
| `XAI_API_KEY`       | authenticates `xai`. Unlike the OpenAI one, it **overrides** a stored xAI OAuth record |
| `AIGEM_HOOKS_DEBUG` | any non-empty value logs every hook invocation to stderr            |
| `XDG_CONFIG_HOME`   | overrides the config directory                                      |
| `XDG_STATE_HOME`    | overrides the state directory                                       |
