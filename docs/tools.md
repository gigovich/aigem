# Tools and subagents

## Built-in tools

The model can call these. All file paths are sandboxed under `--cwd` - see
[the security model](security.md).

| Tool         | Purpose                                                       | Confirmation |
| ------------ | ------------------------------------------------------------- | ------------ |
| `read_file`  | Read a text file, line-numbered for precise edits              | no           |
| `write_file` | Create a file or fully replace its contents                   | yes          |
| `edit_file`  | Replace an exact text fragment in an existing file            | yes          |
| `list_dir`   | List a directory                                              | no           |
| `bash`       | Run a shell command                                           | yes          |
| `grep`       | Regex search over file contents                               | no           |
| `fuzzy_find` | Fuzzy-match file paths                                        | no           |
| `web_search` | Search current public web results (when configured)           | no           |
| `open_url`   | Open a URL in the browser backend, return text + links/forms  | no           |
| `browser_action` | Drive a page: click, type, observe (browser backend only) | no           |
| `todo_write` | Track a multi-step plan (kept until every item is done)        | no           |
| `task`       | Delegate to a specialized subagent                            | no           |
| `skill`      | Invoke a discovered skill                                     | no           |

Not every tool is always present. `web_search` appears once any search backend is
configured; `open_url` and `browser_action` need the **browser** backend
specifically, so they are absent when Brave is the provider. `skill` appears only
when at least one model-invocable skill was discovered.

Bots differ more than that: they get eight extra tools - `memory`, `schedule`,
`save_skill`, `delete_skill`, `post_message`, `handoff`, `read_chat`, and
`team_status` - and they get neither `task` nor `todo_write`.

`write_file`, `edit_file`, and `bash` prompt for confirmation in the TUI before
running. For changes to existing files the model uses `edit_file` (exact
find/replace) so it never has to resend the whole file; `write_file` is for new
files or full rewrites.

### `bash` on Windows

The `bash` tool runs `bash -c`, so on Windows it needs a `bash` on your `PATH` -
Git Bash, WSL, or MSYS2. Without one, every `bash` call fails with
`executable file not found in %PATH%`; the rest of aigem is unaffected.

There is deliberately no automatic fallback to PowerShell or `cmd`. The
destructive-command deny list that backs auto mode and the `shell` capability
profile recognizes Unix command shapes (`rm`, `dd`, `git reset --hard`, ...) and
knows nothing about `del`, `rd /s`, or `Remove-Item`. Silently switching shells
would leave those commands unguarded, so aigem asks for a real `bash` instead.

## Subagents

The model can delegate self-contained work to a specialized subagent via the
`task` tool. Each subagent runs in its own context - its own system prompt,
message history, and restricted tool set - and returns a summary, which keeps the
main conversation's context lean.

| Agent         | Purpose                                            | Tools             |
| ------------- | -------------------------------------------------- | ----------------- |
| `scout`       | Fast read-only codebase recon                      | read-only         |
| `code-writer` | Implement a focused change, then verify it         | read/edit/bash    |
| `simplifier`  | Simplify code while preserving behavior            | read/edit/bash    |
| `reviewer`    | Independent review; reports issues, makes no edits | read-only + bash  |

When the model emits several `task` calls in one response, the subagents run **in
parallel** with bounded concurrency, each in its own context. The TUI shows each
run as its own group - a header with a live spinner (then `✓`/`✗`) and its tool
calls indented beneath - so concurrent runs stay attributed instead of
interleaving. More generally, any independent tool calls the model batches in one
response execute concurrently.

A subagent's confirmations are attributed to it (for example `code-writer › bash`),
and an `Always` approval is scoped to that agent. Concurrent confirmations queue
behind one prompt.

### Adding your own

Drop a markdown file in `~/.config/aigem/agents/*.md` with frontmatter:

```markdown
---
name: my-agent
description: when to use it (shown to the model)
tools: read_file, grep, bash
---
The system prompt for the agent goes here.
```

### Measuring delegation

Whether the machinery works is a unit-test question, and it is covered. Whether
the model *chooses* to delegate - and picks the right agent, and batches
independent calls into one response - is a property of the prompt, and it needs
a measurement rather than an opinion.

`--trace-json` records a `-p` run as JSONL: every tool call, every delegation,
and, uniquely, which calls the model emitted **together** in one assistant
message. Three `task` calls in three consecutive responses look identical to
three in one until you record that grouping, and only the second is parallel.

```sh
aigem -p 'explore each service under services/' --trace-json run.jsonl
```

The file holds the prompt, the final answer, and each tool's arguments and
result clipped to 400 bytes, so it is written `0600` like the credential store.

`evals/` builds on it: fixture workspaces, scenarios that declare whether
delegation is required, forbidden, or optional, and a runner that reports
recall, precision, agent-type accuracy, and parallel compliance across repeated
runs. See [`evals/README.md`](https://github.com/gigovich/aigem/blob/main/evals/README.md).
