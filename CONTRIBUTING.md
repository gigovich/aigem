# Contributing to aigem

Thanks for taking the time. This is a small project, so the process is light.

## Getting set up

```sh
git clone https://github.com/gigovich/aigem
cd aigem
go build ./cmd/aigem
go test ./...
```

That build deliberately produces a binary with no browser UI - the bundle is not
committed, so installing aigem never requires a Node toolchain. If you are
working on the UI you also need Node 22 or newer, and `make web && make build`.

Go 1.26 or newer is required. `go.mod` also carries a `toolchain go1.26.6`
directive, so the Go tool fetches that patch release automatically - it contains
standard-library security fixes that affect aigem.

You do not need a model to work on most of the codebase; the test suite is fully
self-contained and never talks to a network.

## Before you open a pull request

Run what CI runs:

```sh
make check
```

That is `fmt-check`, `vet`, `lint`, `race`, and `cross`. `make help` lists every
target. CI additionally runs `govulncheck`, checks that `go mod tidy` is a no-op,
and lints under `GOOS=windows` - `make check-all` covers those too, if you have
`govulncheck` installed. CI also compiles and vets on a real Windows runner,
which nothing local can reproduce.

`make check` is Go only. The browser UI is linted, typechecked and tested by its
own CI job, and locally by:

```sh
make web-check
```

`make check-all` runs that too when `npm` is on your PATH, and skips it when it
is not.

If you would rather run them by hand:

```sh
make fmt-check        # not `gofmt -l .`: that walks node_modules, where an
                      # npm dependency ships Go files of its own
go vet ./...
go test -race ./...
golangci-lint run     # see .golangci.yml
```

`golangci-lint` is not vendored. Install it with:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

CI also cross-builds every released target, so if you touch anything
platform-specific check it still compiles - `make cross` does this. Process
handling is the usual culprit; the platform-specific pieces live in
`procgroup_unix.go` / `procgroup_windows.go` under `internal/hooks` and
`internal/local`.

## Documentation

The site at [gigovich.github.io/aigem](https://gigovich.github.io/aigem/) is built
from [`docs/`](docs/) with [MkDocs Material](https://squidfunk.github.io/mkdocs-material/).
Preview it locally:

```sh
pip install -r docs/requirements.txt
make docs        # serves on http://127.0.0.1:8000
```

A push to `main` that touches `docs/`, `mkdocs.yml`, or the workflow rebuilds and
deploys it. Pull requests build it too, with `--strict`, so a broken internal
link fails the check instead of silently shipping. New pages must be added to the
`nav:` in `mkdocs.yml` - a page that is not in the nav is a build warning, and
under `--strict` a build failure.

## House style

- **Comments explain "why", not "what".** The code should already say what it
  does. Most functions need no comment at all; the ones that do usually need it
  because a constraint is not visible from the signature.
- **Keep lines under 120 characters.**
- **No new dependencies without a reason.** The dependency list is deliberately
  short. If a stdlib solution is within a few dozen lines, prefer it.
- **Tests go next to the code.** Table-driven where it helps, plain where it
  does not. Anything touching the filesystem uses `t.TempDir()`.

## Where things live

See [docs/architecture.md](docs/architecture.md) for the package map. The short
version:

| Path             | What it is                                          |
| ---------------- | --------------------------------------------------- |
| `cmd/aigem`      | CLI entry point and subcommands                     |
| `internal/agent` | the model/tool loop, budgets, compaction, subagents  |
| `internal/llm`   | provider registry, backends, retry, usage accounting |
| `internal/tools` | the built-in tools and the capability profiles       |
| `internal/tui`   | the Bubble Tea front-end                            |

## Security-sensitive areas

Some parts of the codebase are the security boundary, and changes there get
looked at harder:

- `internal/tools` - the path sandbox and destructive-command deny list
- `internal/pathgrant`, `internal/trust` - what the user has approved
- `internal/hooks`, `internal/skill`, `internal/mcp` - anything that executes
  project-supplied configuration

If you are changing one of these, say so in the PR description and explain what
the new trust boundary is. Do not report vulnerabilities in a public PR - see
[SECURITY.md](SECURITY.md).

## Commit messages

Conventional-commit prefixes (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`,
`ci:`, `chore:`) are used to group the generated release notes. Not enforced,
but appreciated.

## Reporting bugs

Open an issue with the version (`aigem version`), your OS, the model/provider,
and what you expected instead. A minimal reproduction beats a description.
