# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Windows support. The process-group handling in the hook runner and the local
  llama.cpp daemon now has platform-specific implementations, so aigem builds and
  runs on Windows (amd64 and arm64) alongside Linux and macOS. The `bash` tool
  and shell hooks still require a `bash` on `PATH` there - see
  [docs/tools.md](docs/tools.md#bash-on-windows) for why there is no automatic
  PowerShell fallback.
- `aigem version` reports a real version, stamped at release time, and also
  accepts `--version` / `-v`. Unstamped `go install` builds fall back to the
  module version from the build info.
- Published release artifacts: cross-platform binaries with checksums, and a
  multi-arch container image on GitHub Container Registry.
- Documentation site at [gigovich.github.io/aigem](https://gigovich.github.io/aigem/).

### Security

- Updated `golang.org/x/text` (infinite loop, GO-2026-5970), `goldmark` (XSS,
  GO-2026-5320), and `golang.org/x/net`, and added a `toolchain go1.26.5`
  directive for the crypto/tls ECH privacy leak (GO-2026-5856). All three were
  reachable from aigem's own call paths. `govulncheck` now runs in CI.
- Fixed a sandbox escape. The path check resolved symlinks for a path and its
  immediate parent only, so a path with two or more not-yet-existing components
  under a symlinked directory stayed unresolved, passed the containment check,
  and let `write_file` create files outside the working directory. A cloned
  repository can ship such a symlink. Resolution now walks to the deepest
  existing ancestor, and the case is covered by tests.
- "Always (this folder)" no longer over-grants. Approving a *directory* recorded
  its parent, so allowing `/srv/data` silently granted `/srv` and every sibling.
  The grant is now the directory the confirmation box named.
- On Windows, stopping the local daemon verifies the process image against the
  configured binary before terminating it. Windows recycles PIDs, so a stale
  pidfile could otherwise have killed an unrelated process. A refused stop is now
  reported instead of silently deleting the pidfile and orphaning the daemon.
- Documented that both subscription logins reuse the vendor's own CLI OAuth
  client and undocumented endpoints, and that API keys are the supported
  alternative. Previously only the OpenAI path carried any warning.

### Fixed

- A normal `Ctrl+C` shutdown of `aigem bot start` no longer exits non-zero. The
  context-cancellation check compared errors with `!=`, which fails once the error
  is wrapped.
- Hook exit codes are no longer lost when the underlying error is wrapped. The
  runner used a bare type assertion instead of `errors.As`, so a wrapped
  `*exec.ExitError` fell through to the generic failure path and reported `-1`.
- Filesystem errors from the tool layer now wrap their cause with `%w`, so
  callers can use `errors.Is` against them.
- Replaced deprecated Bubble Tea viewport calls (`LineUp`, `LineDown`, `ViewUp`,
  `ViewDown`) with their supported equivalents.
- A hook that sets `shell` now gets the matching "run this string" flag, so
  `cmd` receives `/c` and `powershell`/`pwsh` receive `-Command` instead of a
  `bash`-style `-c` they reject.
- A container started with neither a command nor `BOT_NAME` exits non-zero
  instead of reporting success to whatever supervises it.

### Changed

- The module path is now `github.com/gigovich/aigem`, so
  `go install github.com/gigovich/aigem/cmd/aigem@latest` works.
- The README is a landing page; the reference material it used to carry now lives
  in [`docs/`](docs/index.md).
- Removed dead code: an unused sandbox path resolver, an unused turn-budget
  predicate, and two unused TUI formatting helpers.

### Removed

- Internal deployment scaffolding that was specific to the author's
  infrastructure: the Helm chart, the Gitea CI workflow, and internal design and
  evaluation documents.

[Unreleased]: https://github.com/gigovich/aigem/commits/main
