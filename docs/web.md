# The browser UI

`aigem web` serves the UI on a loopback port and prints the address.

```sh
aigem web                        # a port chosen by the kernel
aigem web --addr 127.0.0.1:7777  # a fixed one
aigem web --open                 # open it in the default browser too
```

This is the first phase of the rewrite: the daemon, the application shell and
the build integration are in place; the screens are not. `/healthz` reports
whether the binary carries a UI at all.

## Release binaries have no UI

Building the bundle needs a Node toolchain, and neither the release pipeline nor
`go install` depends on one - so a downloaded binary and `go install
github.com/gigovich/aigem/cmd/aigem@latest` both answer every page with a 501
naming the missing step. That is deliberate: installing aigem must never require
Node.

From a checkout:

```sh
make web && make build
```

`make web` writes into `internal/web/dist`, which the binary embeds at compile
time. Only a `.gitkeep` is committed there.

## Reaching it from another device

The daemon **refuses** to bind an address the network can reach, and there is no
flag to override it. An origin check needs the public URL the daemon is reached
at, and nothing in a request can be trusted to supply it - `X-Forwarded-Host` is
written by whoever is talking to you. So the address it binds is one only this
machine can reach, and exposure is a decision made outside the process:

```sh
tailscale serve --bg 7777
```

Put any reverse proxy you already trust in front of it; the daemon is not the
thing deciding who may connect.

## What it serves

| Path        | |
| ----------- | --------------------------------------------------------------- |
| `/healthz`  | `{"ok":true,"ui":true}` - `ui` is false on a binary built without one |
| `/api/...`  | reserved; an unknown path here is a 404, never the page |
| everything else | the application, which routes in the browser |

Every response carries a content security policy, `X-Content-Type-Options:
nosniff` and `Referrer-Policy: no-referrer` - including the page and the bundle,
which are the responses the policy exists for. The agent reads pages an attacker
may have written and the UI renders model output, so `img-src` and `form-action`
are load-bearing rather than defence in depth: an `<img>` pointing at an outside
host and a `<form>` posting to one are both exfiltration with no script involved.

## Working on it

```sh
make web-dev       # Vite dev server with hot reload
make web-check     # lint, typecheck and test the UI
```

The dev server proxies `/api` and `/healthz` to a running daemon on
`127.0.0.1:7777`, so start one with `aigem web --addr 127.0.0.1:7777` in another
terminal, or point `AIGEM_ADDR` at wherever it landed.

The sources are under `internal/web/_ui`. The leading underscore is not
decoration: the Go tool skips directories named that way, and without it
`go build ./...` walks `node_modules` and compiles whatever Go files an npm
dependency happens to ship.
