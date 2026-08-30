# Getting started

## Install

The quickest route, if you have **Go 1.26 or newer**:

```sh
go install github.com/gigovich/aigem/cmd/aigem@latest
```

Otherwise grab a binary for your platform from the
[latest release](https://github.com/gigovich/aigem/releases/latest) and put it on
your `PATH`. Release archives ship with a `checksums.txt` worth verifying.

Building from a clone works too:

```sh
git clone https://github.com/gigovich/aigem
cd aigem
go build -o bin/aigem ./cmd/aigem
./bin/aigem
```

Prebuilt binaries are published for Linux, macOS, and Windows on both amd64 and
arm64.

## Requirements

- **Go 1.26+**, if you are building from source or installing with `go install`.
  Prebuilt release binaries need no toolchain.
- **A model.** Either a hosted provider (OpenAI, xAI, or anything OpenAI-compatible
  you configure) or a local [llama.cpp](https://github.com/ggml-org/llama.cpp)
  server. See [Models and providers](models.md).

For the local path you need a `llama-server` binary on your `PATH`. aigem can set
up and launch it for you, so you do not need one already running. The
OpenAI-compatible endpoint at `/v1/chat/completions` with the `--jinja` template
is what makes native tool calling work.

### On Windows

aigem builds and runs on Windows, but three things default to invoking `bash`, so
they need one on your `PATH` - Git Bash, WSL, or MSYS2:

- the `bash` tool,
- hooks that run a shell command (unless the hook sets `shell:`),
- skills that use dynamic `` !`cmd` `` injection (unless the skill sets
  `shell: powershell`).

The TUI, file tools, search, models, and MCP work without it. See
[the note in Tools](tools.md#bash-on-windows).

The default local server address is `http://127.0.0.1:9280`.

## First run

Just run it:

```sh
aigem
```

Before the TUI opens, aigem asks you to set up a web-search backend (a Brave API
key, or Chrome automation). Search is optional and declining it starts aigem
normally - but skipping records nothing, so the prompt returns on every
interactive launch until you either configure a backend or turn it off:

```sh
aigem search set brave --api-key-stdin   # or: aigem search set browser
aigem search clear                       # explicitly no search backend
```

See [Web search](search.md).

With no `--model`, aigem reuses the model you last selected, then falls back to
the first authenticated provider, then to the local model. If nothing is set up,
the TUI shows a "Local model not set up" alert with a **Set up & download**
action - nothing is downloaded until you choose it.

To log in to a hosted provider first:

```sh
aigem auth login openai        # ChatGPT subscription, via browser OAuth
aigem models                   # what is available now
```

## Non-interactive mode

`-p` runs one prompt and exits. The final answer goes to stdout and tool activity
goes to stderr, so you can capture just the answer:

```sh
aigem -p 'how many .go files are here? answer with just the number'
```

`-p` uses the `workspace-write` capability profile by default: read/search tools
and `write_file`/`edit_file` are available, but `bash` is not exposed. Confirm-gated
tools are still denied unless you pass `-y`.

Shell workflows must opt in explicitly:

```sh
aigem -y --capability-profile shell -p 'count the .go files with a shell command, reply COUNT=<n>'
```

See [the security model](security.md) for what each profile permits - this is the
part worth understanding before automating anything.

## Flags

| Flag                     | Default                 | What it does                                                        |
| ------------------------ | ----------------------- | ------------------------------------------------------------------- |
| `--url`                  | `http://127.0.0.1:9280` | llama.cpp base URL                                                  |
| `--model`                | *(auto)*                | `provider/id` (e.g. `openai/gpt-5.6-sol`), a bare local model name   |
| `--cwd`                  | `.`                     | working directory, and the sandbox root                             |
| `--temp`                 | `0.3`                   | sampling temperature                                                |
| `--max-tokens`           | `8192`                  | cap tokens per response (`0` = no cap)                              |
| `--ctx-size`             | `262144`                | model context window, used by the usage gauge                       |
| `-p`                     | -                       | run a single prompt non-interactively and exit                      |
| `-y`                     | `false`                 | auto-approve confirm-gated tools in `-p`, within the active profile  |
| `--capability-profile`   | `workspace-write`       | `read-only`, `workspace-write`, `shell`, or `dangerous-shell`        |
| `--trace-json`           | -                       | record a `-p` run's tool and delegation activity as JSONL            |
| `--repl`                 | `false`                 | plain line-based REPL instead of the TUI                            |

`aigem version` is a subcommand, not a flag, and prints the version. `--version`
and `-v` do the same but only as the first argument.

### Compaction

| Flag                    | Default | What it does                                       |
| ----------------------- | ------- | -------------------------------------------------- |
| `--compact-auto`        | `true`  | enable automatic context compaction                |
| `--compact-at-pct`      | `70`    | context % at which to summarize older turns        |
| `--evict-at-pct`        | `50`    | context % at which to evict old tool output        |
| `--compact-keep-turns`  | `10`    | recent messages kept verbatim across summarization |
| `--compact-keep-tools`  | `4`     | recent tool results kept verbatim during eviction  |

### Unattended turn budgets

These bound a `-p` turn so a loop cannot run forever. Set any to `0` to
disable it.

| Flag                       | Default | What it bounds                     |
| -------------------------- | ------- | ---------------------------------- |
| `--turn-timeout`           | `20m`   | wall-clock time for one turn       |
| `--max-model-rounds`       | `40`    | model rounds for one turn          |
| `--max-tool-calls`         | `120`   | total tool calls for one turn      |
| `--max-repeat-tool-calls`  | `8`     | identical tool calls for one turn  |

### Project trust

| Flag                       | What it approves                                              |
| -------------------------- | ------------------------------------------------------------- |
| `--trust-project-hooks`    | the current project-local hook configuration                  |
| `--trust-project-skills`   | the current project-local skill definitions                   |
| `--trust-project-mcp`      | the currently configured project-local MCP servers and policies |

See [Security model](security.md) for what these mean and why they are separate.
