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
the build integration are in place; the screens are not. `/healthz` says only
that the process is up; `GET /api/meta`, behind the credential, is where a page
reads the version, the default model and which features this build serves.

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
| `/healthz`  | `{"ok":true}`, unauthenticated: a liveness probe and nothing else |
| `POST /api/auth/session` | trades the token for a cookie, or renews one close to expiry |
| `DELETE /api/auth/session` | signs this browser out, on the daemon as well as in the browser |
| `GET /api/meta` | version, default model, `ui`, the feature map, and the revision it is at |
| `GET /api/socket` | the control stream: what changed elsewhere in the daemon |
| `GET /api/runs` | the conversations this daemon knows about, oldest first |
| `POST /api/runs` | opens one; answers 201 with the record |
| `GET /api/runs/{id}` | one record |
| `DELETE /api/runs/{id}` | saves the conversation and ends its session |
| `GET /api/runs/{id}/events` | a page of the timeline, `?since=&limit=` |
| `GET /api/runs/{id}/socket` | the run stream, `?since=` |
| `GET /api/runs/{id}/artifacts` | the files the run changed, both sides |
| `/api/...`  | reserved; an unknown path here is a 404, never the page |
| everything else | the application, which routes in the browser |

A binary built without a bundle is the exception to the last row: it has no page
to serve, so it answers 501 there. `/healthz`, the API routes above and the
wrong-method 405s answer as they always do.

## The control stream

`GET /api/socket` is one websocket per tab, and it only reads. Every mutation
goes over HTTP, where a status code and an error body mean something.

The daemon keeps one revision counter for the whole process. Every published
change takes the next number, and every frame carries the number that connection
is at once it has been read:

```json
{"type":"hello","rev":12,
 "data":{"version":"...","defaultModel":"...","rev":12,"ui":true,"features":{...}}}
{"type":"run.updated","rev":13,"data":{"id":"..."}}
```

`hello` arrives first and is the client's base - byte for byte the document
`/api/meta` serves, so a page that has just reconnected does not have to ask
again. Its `rev` is the same number as the envelope's, which is the point: every
snapshot says which revision it is current as of, or a page that refetched after
a gap could mark itself up to date at a revision the answer predates.

There is no replay. A client that sees a `rev` more than one past the last it
handled has missed something and refetches the collection it cares about. A
frame that is not a change - the refusal of an op this stream will not carry -
repeats the revision that client is already at, so a client's own mistake never
reads as a gap. A delta that arrives with no `data` carries nothing the client
can apply - the daemon had nothing to say, or could not encode what it had - so
treat it as a gap and refetch.

A client that stops reading is disconnected rather than skipped past, because a
skip is only ever noticed by a message that arrives after it, and the burst that
fills a queue is the burst that then ends. Its recovery is a reconnect, which
re-bases it.

The only thing a client may send up is `{"op":"ping"}`. Anything else comes back
as `{"type":"client_error", ...}` and the socket stays open - except a message
past the size limit below, which ends it. A binary frame is discarded in
silence.

The connection's own contract: the daemon sends a protocol ping every 30
seconds, hangs up after 90 seconds without a frame from the client, and caps one
frame at 64 KiB and one message at 256 KiB. A browser answers the ping itself,
so a page has nothing to do to stay connected. The daemon holds 64 websockets at
once across every tab; the 65th handshake is refused with a 503 and a
`Retry-After`, which is a retry rather than an error to show.

## Runs

A run is one conversation. It is the same session the terminal opens - one
event stream, one approval queue, one journal - reached over HTTP instead of
over a keyboard, so a browser and a terminal looking at the same run see the
same thing rather than two renderings that drifted.

A record outlives the daemon; a session does not. `status` is `open` while there
is a session and `closed` once there is not, and `live` is the field a page
reads before it opens a socket. A restarted daemon finds every run closed: the
record and the timeline are still there to read, and nothing is left claiming a
session that went with the process.

A conversation names itself on its first message, and the record catches up
then - so a run listed after a restart carries the name it gave itself and the
id its journal is under.

One daemon holds 32 open runs. The 33rd `POST` is answered `503` with a
`Retry-After` and a body saying how many are open, and closing one gives its
place back: a run is a tools registry, a model handle and an event ring, and a
client looping on create would otherwise be an out-of-memory with nothing in the
way of it. The record of a closed run is not counted and is never removed.

Every change to a run - opened, named by its conversation, switched model,
closed - is announced on the control stream as `run.updated`, carrying the
record. Most of them do not happen during a request: a conversation takes its
name on its first message, which arrives up the run socket, so a page that
listened only for the answers to its own requests would show that run as
untitled until something else moved.

`DELETE` is that transition and not a deletion. It saves the conversation and
ends the session, and the record and the journal stay, because a run somebody is
done with is one they can still look back at. Doing it twice is not an error:
two tabs pressing the same button is the ordinary case.

Statuses a client has to tell apart:

- `404` - no such run.
- `409` - the run has no live session. Its timeline still reads; its artifacts
  and its socket do not, because both live with the session rather than in the
  journal.
- `410` - the history no longer reaches the point asked for. Answered by
  reloading, not by retrying.
- `503` with a `Retry-After` - the daemon is holding as many runs as it will.
  The one refusal here that is worth retrying: nothing about the request is
  wrong, and closing a run makes room.
- `400` - a sentence meant to be shown. It is written for a person, and a
  front-end must render it as text, never as markup.
- `500` - carries nothing. What it was is in the daemon's log.

A browser cannot read the status of a failed websocket handshake: the
`WebSocket` API reports only that it failed. So a page whose socket will not
open finds out why over HTTP - `GET /api/runs/{id}` for a `404` or a `409`, and
`GET /api/runs/{id}/events?since=` for the `410`, which is the request it would
have made anyway.

`?since=` and `?limit=` are non-negative whole numbers. Every cursor on this API
is one: opaque to the client, compared only for equality and order. A timeline
is read in pages of at most 2000 events - an absent `limit`, a zero one and one
past the cap all mean a full page - and a client that gets a full one asks again
from the last sequence it saw, the same loop it uses to catch up after a
disconnect. There is no "more" marker: a page that came back full is the signal
to ask again.

A create body is capped at 16 KiB, and anything past that is a `400`.

`GET /api/runs/{id}/artifacts` lists every file the run changed. The content of
both sides comes with it, but only up to a budget - a run that appended a line
to a generated file holds both copies of it, and a response carrying them would
be several more. Past that, `truncated` is set and the content is left out;
`oldBytes` and `newBytes` are the real sizes either way, so a page can always
say how big a change is.

Booleans that are false are absent rather than present-and-false, the way Go's
`omitempty` writes them - `running`, `waiting`, `step` and `seq` all behave that
way. `live` is the exception and is always there, because it is the field a page
reads before it decides whether to open a socket.

## The run stream

`GET /api/runs/{id}/socket?since=<seq>` is one websocket per open run per tab.
It is the opposite of the control stream in both directions: it replays, and it
takes mutations.

`?since=` is the last sequence the client holds - the `seq` field of the last
event it applied; the backlog is spliced in front of the live events, so there
is no window in which an event is neither replayed nor delivered. `?kind=` and
`?label=` say who is attaching - they are what the presence event shows the
other tabs, and `kind` defaults to `web`. A `since` the run no longer reaches is
refused with a `410`, which a browser sees only as a failed handshake: see
above.

Down come the session's own events, in order and exactly as the session wrote
them. A client that falls too far behind is sent a `desync` event carrying the
last sequence it did get, and then **the socket ends**: recovery is to refetch
over `/api/runs/{id}/events?since=<that sequence>` and dial again, not to keep
waiting on a stream that has already closed.

Up go the client's operations, one JSON document per frame:

```json
{"op":"submit","text":"...","images":[{"media_type":"image/png","data":"..."}]}
{"op":"interrupt"}
{"op":"resolve","id":"a1","decision":"once","label":"web"}
{"op":"command","name":"new","args":""}
{"op":"step_mode","on":true}
{"op":"switch_model","ref":"openai/gpt-5.6-sol","persist":true}
{"op":"ping"}
```

`step_mode` is the toggle a person sees, and `on` means "ask me about every tool
call" - the inverse of the session's auto mode. The run's current setting is the
`step` field of its record, absent when it is off.

`command` runs a slash command inside the conversation. The daemon registers
none yet - every one of them comes back refused as unknown - and the catalog a
palette would offer arrives with `/api/commands`.

`switch_model` with `"persist":true` is the one operation whose effect leaves
the run: it writes the operator's saved model preference, which the next session
started anywhere - including the terminal - will use. It is refused while a turn
is running: the turn already started against the model being replaced, so
switching under it changes nothing about the answer being produced.

An operation that is applied is answered with nothing; the conversation's own
events are the acknowledgement. One that is refused comes back as
`{"kind":"client_error","op":"...","error":"..."}` and the socket stays open: an
approval somebody else answered first is the normal outcome of two people
answering at once, not a failure in the conversation, and it must not appear in
the timeline as one. `ping` is answered with silence.

The connection's own contract - the pings, the timeouts, the frame and message
caps, the 64-socket ceiling - is the control stream's, above. The frame cap is
the one that bites here: a browser sends a message as a single frame, so a
submit may carry 64 KiB. That is ample for typed text and not enough for a
pasted screenshot, so `images` is on the wire and a large one is not yet
something this op can carry.

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
