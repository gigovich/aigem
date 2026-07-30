# Security Policy

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

Report it privately through GitHub's
[private vulnerability reporting](https://github.com/gigovich/aigem/security/advisories/new)
on this repository. Include what you did, what happened, and what you expected -
a proof of concept helps a lot.

Expect an initial response within a week. This is a small project maintained by
one person, so please be patient; you will get an honest answer about whether
and when a fix is coming.

## Supported versions

Fixes land on `main` and go out in the next release. Older tagged releases are
not patched.

## What aigem's security model actually is

aigem runs a language model that can read files and, when you allow it, run
commands. That is the point of the tool, so "the model did something I did not
want" is only a vulnerability when it crossed a boundary aigem claims to
enforce. Those claims are:

- **The filesystem sandbox.** File tools resolve under `--cwd`. Reads outside it
  require an explicit grant; writes outside it are asked about every time and are
  never remembered. A path that escapes the sandbox without the documented
  prompt is a vulnerability.
- **Capability profiles.** Unattended runs (`-p`, bots) use a profile.
  `workspace-write`, the default, does not expose `bash` at all. `-y` approving a
  shell command under a profile that should not expose shell is a vulnerability.
- **The destructive-command deny list.** Some shell commands are refused even
  under `dangerous-shell`. A bypass of that list is a vulnerability.
- **Project trust.** Hooks, skills, and MCP servers defined by a *repository*
  are inert until approved, and the approval is fingerprinted against their
  configuration. Project-supplied configuration executing before approval, or a
  changed configuration running under a stale approval, is a vulnerability.
- **Credential handling.** Tokens live in `~/.local/state/aigem/auth.json` with
  `0600` permissions and are never logged. A credential reaching a host it was
  not stored for - notably via a repo-local `models.json` overriding a
  provider's `base_url` - is a vulnerability.

Things that are **not** vulnerabilities:

- The model running a destructive command you approved at the prompt.
- A bot with `capabilityProfile: dangerous-shell` doing something destructive.
- Prompt injection changing what the model *says*. Prompt injection that makes
  it cross one of the boundaries above **is** in scope, and is the case worth
  reporting.

## Third-party providers

aigem talks to model providers you configure. The ChatGPT-subscription path uses
OpenAI's undocumented Codex backend and is governed by their terms of service;
the API-key path is the supported alternative. Vulnerabilities in a provider's
own service should go to that provider.
