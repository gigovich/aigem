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

## Signing in

The printed URL carries a token: `http://127.0.0.1:7777/?token=...`. The page
spends it once on `POST /api/auth/session`, gets back an `HttpOnly;
SameSite=Strict` cookie, and rewrites the address bar without it - so a reload,
a bookmark or a screenshot does not carry the credential.

Until that exchange happens the token is a secret in plain sight: it is on
stdout, and with `--open` it is in this machine's process table for as long as
the browser takes to start. On a machine you share with people you would not
give a shell to, start the daemon without `--open` and paste the link yourself.

Sign-ins are kept in `$XDG_STATE_HOME/aigem/web-cookies.json` (0600), so
restarting the daemon - deploying a new binary, say - does not sign out the
phone whose token lives in a terminal on another machine. Delete the file while
the daemon is stopped to revoke every one of them; `DELETE /api/auth/session`
revokes just the browser that asks.

Everything except the page itself and `/healthz` needs a credential. The page is
what the browser fetches before it can hold one, and `/healthz` is a liveness
probe that reports nothing the page would not.

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

The simplest shape needs no flag: keep the bind on loopback and put a proxy you
already trust in front of it.

```sh
tailscale serve --bg 7777
```

To bind an address the network can reach, say which public URL the daemon
answers to:

```sh
aigem web --addr 0.0.0.0:7777 --origin https://aigem.example.ts.net
```

Without `--origin` that bind is **refused**. An origin check needs the name the
daemon is reached under, and nothing in a request can be trusted to supply it -
`X-Forwarded-Host` is written by whoever is talking to you. So a person states
it, and the daemon matches both `Host` and `Origin` against it exactly.

Repeat `--origin` for a daemon reached under more than one name. A stated origin
**replaces** the derived allowlist rather than extending it - behind a proxy the
bind address is not the name requests arrive under, and leaving it allowed only
widens what a DNS rebinding attack may claim to be. Loopback names survive the
replacement, so `curl` on the machine itself keeps working.

The scheme is part of the match: `https://name` and `http://name` are different
origins, and a cookie issued under an `https` origin is marked `Secure` even
though the hop to the daemon is plain HTTP.

## What it serves

| Path        | |
| ----------- | --------------------------------------------------------------- |
| `/healthz`  | `{"ok":true,"ui":true}` - `ui` is false on a binary built without one |
| `POST /api/auth/session` | trades the token for a cookie, or renews one close to expiry |
| `DELETE /api/auth/session` | signs this browser out, on the daemon as well as in the browser |
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
terminal, or point `AIGEM_ADDR` at wherever it landed. The proxy sends the
daemon's own origin rather than `http://localhost:5173`, so the dev cycle needs
no `--origin`. Sign in by opening the dev server once with the token the daemon
printed: `http://localhost:5173/?token=...`.

The sources are under `internal/web/_ui`. The leading underscore is not
decoration: the Go tool skips directories named that way, and without it
`go build ./...` walks `node_modules` and compiles whatever Go files an npm
dependency happens to ship.
