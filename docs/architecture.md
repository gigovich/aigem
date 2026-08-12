# Architecture

A map of the codebase for contributors. See
[CONTRIBUTING.md](https://github.com/gigovich/aigem/blob/main/CONTRIBUTING.md) for
the workflow.

## Packages

| Package              | Responsibility                                                        |
| -------------------- | --------------------------------------------------------------------- |
| `cmd/aigem`          | entry point, flag parsing, and the `auth`/`mcp`/`models`/`bot`/`search`/`paths`/`usage` subcommands |
| `internal/agent`     | the model/tool loop, turn budgets, context compaction, subagent delegation |
| `internal/llm`       | backend interface, chat-completions and Responses adapters, model registry, retry, usage accounting |
| `internal/auth`      | credential store, the ChatGPT OAuth flow, and the xAI device-code flow |
| `internal/tools`     | tool registry, the sandboxed tools, capability profiles, destructive-command deny list |
| `internal/pathgrant` | persisted approvals for read paths outside the working directory      |
| `internal/trust`     | fingerprinted approvals for project-supplied hooks, skills, and MCP targets |
| `internal/hooks`     | lifecycle hook discovery and execution                                |
| `internal/skill`     | Agent Skill discovery, frontmatter, and body rendering                |
| `internal/mcp`       | MCP client, transports, and OAuth                                     |
| `internal/search`    | Brave and browser-driven web search, plus `open_url` and `browser_action` |
| `internal/config`    | system-prompt assembly and path resolution                            |
| `internal/session`   | conversation persistence for `/resume`                                |
| `internal/local`     | the local llama.cpp server: config, daemon lifecycle, download progress, health (the setup wizard itself lives in `cmd/aigem`) |
| `internal/tui`       | the Bubble Tea front-end                                              |
| `internal/bot`       | unattended bots, roles, memory, cron, and the store adapter           |

Roughly 28k lines of non-test Go, plus about 17k lines of tests.

## How a turn flows

1. A front-end (`tui`, the `--repl` loop, or `-p` in `cmd/aigem`) hands a prompt to
   `agent`.
2. `cmd/aigem` assembles the system prompt via `config` - built-in or `SYSTEM.md`,
   plus the discovered project instruction files - and hands it to the agent
   along with any `SessionStart` hook context.
3. `agent` calls the selected backend through `llm`, which handles streaming,
   retries, and usage accounting.
4. Tool calls come back and are dispatched through `tools`. Each one passes
   through `hooks` (`PreToolUse`), then the front-end's confirmation, then the
   tool itself - the path sandbox and `pathgrant` live *inside* the tool's own
   resolution step - and finally `hooks` again (`PostToolUse`). The capability
   profile is not per call: it is applied once at startup, by building the
   registry as a subset.
5. Independent tool calls in one response run concurrently. A `task` call spawns a
   subagent with its own context and restricted toolset.
6. When pressure crosses the configured thresholds, `agent` compacts the
   conversation - evicting old tool output, then summarizing older turns.
7. The TUI persists the turn via `session`, which is what `/resume` reads back.
   `-p` and `--repl` are stateless and save nothing.

## Design constraints worth knowing

- **The sandbox is in `tools`, not the front-end.** Every front-end gets the same
  enforcement; the front-end only decides *how to ask*.
- **Unattended paths never escalate.** `-p` and bots resolve their capabilities
  from a profile up front. There is deliberately no code path that lets an
  unattended run acquire a capability mid-turn.
- **Project-supplied configuration is inert until fingerprinted and approved.**
  `trust` is the single place that decides; `hooks`, `skill`, and `mcp` ask it.
- **The local daemon outlives the process.** `local` spawns `llama-server` in its
  own process group so it survives aigem exiting and is reused next launch. The
  platform-specific parts are in `procgroup_unix.go` / `procgroup_windows.go`.

## Platform support

Linux, macOS, and Windows all build and are cross-compiled in CI, on amd64 and
arm64. The test suite runs on Linux and macOS; the hook runner shells out to
`bash`, so the suite is not meaningful on Windows and CI compiles and vets there
instead.
