# The browser UI

`aigem web` serves the UI and prints the address to open. It binds a loopback
port unless `--origin` says otherwise; behind a reverse proxy the printed
address is the public one, not the bound one.

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
phone whose token lives in a terminal on another machine.

`--sign-out` refuses to serve at all when it cannot forget the sessions - an
unwritable state directory, or none it can find. A revocation that quietly did
not happen is the one answer worth refusing to start over.

That cuts both ways, and it is the one thing to know about this design:
**restarting is not how you revoke a leaked token.** A restart mints a new
token and leaves every cookie working, and a cookie renews itself for as long
as it is used. If the token got out, stop the daemon and start it with
`--sign-out`, which forgets every session first. Stopping it is not optional: a
daemon still running holds the sessions in memory, goes on honouring every
cookie, and records its next change against the file `--sign-out` removed.
`DELETE /api/auth/session` revokes just the browser that asks.

One more thing the cookie inherits from being a cookie: browsers do not scope
cookies by port. Any other service you visit on the same loopback host receives
the aigem session cookie in its request headers, and that value is a working
credential. The token in the URL is not sent anywhere, but the cookie it buys
is - so a loopback daemon is only as private as the other things listening on
that host.

Everything except the page, `/healthz` and the wrong-method 405s needs a
credential, and those are outside the origin check as well: the page is what a
browser fetches before it can hold any credential, `/healthz` is a liveness
probe, and a 405 carries nothing but an `Allow` header. They are served under
any `Host`, so on a daemon bound where the network can reach, the bundle and a
liveness bit are readable by anyone who can connect. Everything behind them -
every `/api/` route that does anything - is not.

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
it, and the daemon matches both `Host` and `Origin` against it exactly - case
and a trailing root dot aside, which are the same name to a resolver.

A request that carries no `Origin` at all is allowed on the token alone. That is
not a browser page: browsers send the header on every request that could change
anything, and `SameSite=Strict` keeps the cookie off cross-site requests
regardless. It is what lets `curl` work.

An internationalised name has to be given in the punycode form a browser sends
(`https://xn--r8jz45g.jp`); the unicode spelling is refused at startup, because
it would match nothing and every request would 403.

With `--origin` the printed link is the public one, and the address the daemon
actually bound goes to stderr - which is the only way to learn it when the port
was left to the kernel. That line is the bound socket as the kernel reports it,
so a `0.0.0.0` bind on a dual-stack host reads back as `[::]`, and it is left
out when it would repeat the link.

Repeat `--origin` for a daemon reached under more than one name. A stated origin
**replaces** the derived allowlist rather than extending it - behind a proxy the
bind address is not the name requests arrive under, and leaving it allowed only
widens what a DNS rebinding attack may claim to be.

The loopback names survive that replacement, so `curl` and a browser on the
machine itself keep working - but only the ones the socket actually answers on.
A daemon bound to a loopback address or a wildcard keeps `127.0.0.1`, `[::1]`
and `localhost` as they apply; one bound to a routable address keeps nothing,
and answers to the stated origin alone, including from the machine it runs on.

The scheme is part of the match: `https://name` and `http://name` are different
origins, and a cookie issued under an `https` origin is marked `Secure` even
though the hop to the daemon is plain HTTP.

## What it serves

| Path        | |
| ----------- | --------------------------------------------------------------- |
| `/healthz`  | `{"ok":true,"ui":true}`, unauthenticated; `ui` is false with no bundle |
| `POST /api/auth/session` | trades the token for a cookie, or renews one close to expiry |
| `DELETE /api/auth/session` | signs this browser out, on the daemon as well as in the browser |
| `/api/...`  | reserved; an unknown path here is a 404, never the page |
| everything else | the application, which routes in the browser |

A binary built without a bundle is the exception to the last row: it has no page
to serve, so it answers 501 there. `/healthz`, the two `/api/auth/session`
methods and the wrong-method 405s answer as they always do.

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
terminal, or point `AIGEM_ADDR` at wherever it landed - **as a full origin**,
`http://127.0.0.1:9000` rather than `127.0.0.1:9000`. The value is both the
proxy target and the `Origin` it sends, and without a scheme the proxy cannot
resolve the target at all: every request gets Vite's own 502 and the daemon
never sees it.

The proxy sends the daemon's own origin rather than `http://localhost:5173`, so
the dev cycle needs no `--origin`. Sign in by opening the dev server once with
the token the daemon printed: `http://localhost:5173/?token=...`.

The sources are under `internal/web/_ui`. The leading underscore is not
decoration: the Go tool skips directories named that way, and without it
`go build ./...` walks `node_modules` and compiles whatever Go files an npm
dependency happens to ship.
