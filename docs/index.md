# aigem

A terminal AI coding agent written in Go, with a [Bubble Tea](https://github.com/charmbracelet/bubbletea)
TUI. It runs a model against a small set of sandboxed tools, and it works with a
model you host yourself as happily as with a hosted one.

## Start here

- **[Getting started](getting-started.md)** - install, first run, and every flag.
- **[Models and providers](models.md)** - local llama.cpp, OpenAI, xAI, your own
  endpoints, and how quota is tracked.
- **[Security model](security.md)** - the sandbox, capability profiles, path
  grants, and project trust. Worth reading before you point it at a repository.

## Reference

| Page                                 | What it covers                                              |
| ------------------------------------ | ----------------------------------------------------------- |
| [Tools](tools.md)                    | The built-in tools and the `task` subagents                 |
| [Skills](skills.md)                  | Claude-compatible Agent Skills                              |
| [Hooks](hooks.md)                    | Lifecycle hooks that observe, block, or rewrite a turn      |
| [MCP servers](mcp.md)                | Connecting Model Context Protocol servers                   |
| [Web search](search.md)              | Brave and browser-driven search backends                    |
| [The TUI](tui.md)                    | Keys, sessions, compaction, runaway guards, theming         |
| [Configuration](configuration.md)    | System prompt overrides and project instruction files       |
| [Chat bots](bots.md)                 | Unattended bots over Mattermost                             |
| [Docker](docker.md)                  | Running a bot in a container                                |
| [Architecture](architecture.md)      | Package map, for contributors                               |

## What it is not

aigem is a personal tool that grew into something worth publishing. It is not a
product, there is no telemetry, and it makes no attempt to be a drop-in
replacement for any commercial agent. What it does have is a real sandbox, an
honest trust model, and no network calls you did not configure.
