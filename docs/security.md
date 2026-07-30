# Security model

aigem runs a model that can read your files and, if you let it, run commands.
This page describes exactly what is enforced, so you can decide how much rope to
hand it. To report a vulnerability, see
[SECURITY.md](https://github.com/gigovich/aigem/blob/main/SECURITY.md).

## Capability profiles and approvals

**Interactive sessions** (the TUI and `--repl`) expose the full toolset and ask
before confirm-gated tools. `Shift+Tab` auto mode approves edits and
non-destructive shell commands, but still stops for irreversible actions.

**Unattended sessions** (`-p` and bots) have nobody to ask, so they use capability
profiles instead:

| Profile            | Read/search | File writes | `bash`                      |
| ------------------ | ----------- | ----------- | --------------------------- |
| `read-only`        | yes         | no          | no                          |
| `workspace-write`  | yes         | yes         | no                          |
| `shell`            | yes         | yes         | non-destructive only        |
| `dangerous-shell`  | yes         | yes         | yes, minus hard-deny patterns |

`workspace-write` is the default. Under it, `-y` can approve file edits but
cannot silently approve shell, because `bash` is not in the toolset at all.

`shell` is the explicit opt-in for unattended non-destructive shell; destructive
commands (`rm`, `git reset --hard`, `git clean`, ...) are still denied.
`dangerous-shell` is the opt-in for unattended destructive shell - and even then,
catastrophic hard-deny patterns are blocked by the `bash` tool itself.

### Turn budgets

Unattended turns also carry runaway budgets: 20 minutes wall-clock, 40 model
rounds, 120 tool calls, and 8 identical tool calls. Exhausting one returns a
final `Budget exhausted: ...` answer and emits a notice. Tune them with
`--turn-timeout`, `--max-model-rounds`, `--max-tool-calls`, and
`--max-repeat-tool-calls`, or set a value to `0` to disable that budget.

## The filesystem sandbox

File tools resolve under `--cwd`. Path traversal is blocked, including through
symlinks.

When a tool asks for a path outside the sandbox, an interactive session does not
refuse outright: the confirmation box names the path and offers `Once`,
`Always (this folder)`, and `Deny`. `Always` records that file's directory, and
everything under it, as readable from this working directory - in this session
and later ones. `--repl` asks the same question on stdin.

**Grants cover reads only.** A `write_file` or `edit_file` outside the working
directory is asked about every time and is never remembered, so its box offers
only `Once` and `Deny`.

Grants are scoped to the working directory they were made from, and stored in
`~/.local/state/aigem/path-grants.json`:

```sh
aigem paths                 # what this working directory may also read
aigem paths list --all      # every working directory's grants
aigem paths forget <dir>    # drop one
aigem paths forget --all    # drop all of this working directory's grants
```

Unattended sessions behave differently, deliberately:

- **Bots** have nobody to ask and do not read the grant file at all. A directory
  you approved interactively never opens for a bot in the same working directory.
- **`-p`** honors existing grants but cannot create new ones, and refuses
  anything not already granted.

## The `bash` caveat

The sandbox constrains the *file tools*. The `bash` tool runs a real shell, so it
is not sandboxed beyond its working directory - a command can touch anything your
user can. The deny lists catch the catastrophic cases, not every bad idea.

Only approve `bash` and `write_file` calls you actually understand. This is the
part of the model the deny list cannot do for you.

## Project trust

A repository you clone can carry configuration that *executes*: hooks, skills,
and MCP servers. None of it runs until you approve it.

Approvals are stored in `~/.local/state/aigem/project-trust.json`, scoped to the
canonical project path, the specific capability and target, and a SHA-256
fingerprint of its effective configuration. Editing an approved hook, skill, MCP
command/URL/credential, or approval policy invalidates **only** the affected
approval - it never silently authorizes the new configuration.

The five capabilities are approved separately: hooks, skills, stdio MCP servers,
HTTP MCP targets, and MCP `autoApprove` policy. In particular,
`--trust-project-mcp` approves each current stdio/HTTP target and each declared
`autoApprove` policy separately; it is not a permanent whole-project trust bit. A
transport can stay allowed while a changed approval policy is invalidated, in
which case the server starts with `autoApprove` disabled until you approve the
new policy.

HTTP approval is per named target and fingerprint, and never grants general
network access.

Project instruction files (`AGENTS.md`, `CLAUDE.md`) are prompt-only text. They
neither receive nor confer executable trust.

Entries in the legacy `trusted-hooks.json` store migrate lazily at first use, so
existing installations keep working without widening one migrated approval to
other capabilities or to targets added later. New approvals are only ever written
to `project-trust.json`.

## Credentials

Credentials are stored `0600` in `~/.local/state/aigem/auth.json` and are never
logged.

A project-local `.aigem/models.json` may add providers and tweak an existing
provider's models, but never its `base_url`, `api`, `auth`, or `headers`. That
file ships with a possibly-cloned repository, and must not be able to aim a
credential you stored at another host.

Pinned bot models are resolved only against the built-in providers and
`~/.config/aigem/models.json`, never a repo's own `models.json`.
