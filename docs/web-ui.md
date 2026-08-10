# Web UI (design)

!!! note "Partly built"

    Steps 0 to 10 of the work breakdown have landed, in part, and have been
    through a review pass: call ids on tool events,
    `internal/uisession`, the terminal ported onto it, the journal, and the
    daemon with its protocol. There is no UI yet: `aigem web run` serves the API
    and says so. The design below is kept in step with the code as it is
    written, and the places where building it changed the design say so.

aigem's terminal front-end is one way to drive the agent, not the only possible
one. This design adds a second, independent front-end: a browser UI served by a
long-lived local daemon, reachable from a laptop and from a phone on the same
tailnet.

The goal is not to put the TUI in a browser. It is to have a front-end where
diffs, file trees, dashboards and push notifications are natural, and to make
that the place new features land first.

## Shape

Three processes, two of which are optional.

```
aigem                      TUI, own session, own process, no daemon involved
aigem web run              daemon: owns sessions, serves the browser UI
aigem attach <session-id>  TUI as a client of the daemon
```

The daemon owns the sessions because the mobile story requires it: start a turn
on the laptop, close the lid, open the phone, watch it finish. A session that
lives in a terminal process cannot do that.

A browser and a TUI may be attached to the same session at once. That falls out
of the design rather than being built for; see [Approvals](#approvals).

`aigem attach` finds the daemon the same way the local model daemon is found
today: a small state file next to `local.json`.

```
$STATE_DIR/web.json   {"pid": ..., "addr": "127.0.0.1:9290", "token": "..."}
```

It is written on start, removed on clean exit, and treated as stale when the
pid is gone. `aigem web run` refuses to start a second daemon over a live one.

## `internal/uisession`

Today `internal/tui` holds both the rendering and the session logic: slash
commands, model switching, compaction, session persistence, the confirmation
queue, artifact tracking. A second front-end would have to duplicate all of it,
and the two would drift within a month.

So the logic moves into `internal/uisession`, and `Session` is an interface:

```go
type Session interface {
    // Subscribe attaches a client and returns its event channel plus a function
    // to detach. The backlog after since is loaded before the subscriber goes
    // live, so no event falls between the replay and the tail.
    Subscribe(c Client, since uint64) (<-chan Event, func(), error)
    // Replay returns journalled events after since, for a client that is not
    // holding a subscription open.
    Replay(since uint64) ([]Event, error)

    Submit(text string, images []llm.Image) error
    Interrupt()
    Command(name, args string) error
    // Resolve answers the open approval; by labels who answered.
    Resolve(id string, d Decision, by string) error

    // Meta identifies the conversation; Pending is the approval blocking it,
    // which a client attaching mid-turn never saw asked.
    Meta() session.Meta
    Pending() (string, *Approval)
    Close()
}
```

Two implementations:

- **`uisession.Local`** runs the agent in this process. Used by `aigem` and by
  the daemon.
- **`uisession.Remote`** speaks the websocket protocol below. Used by
  `aigem attach`.

`internal/tui` is written against the interface and keeps only rendering, key
handling, layout and the viewport. It gets `attach` for free: there is no second
protocol and no RPC layer, because the event stream *is* the protocol.

The existing TUI test suite is the acceptance criterion for the extraction. It
covers the behaviour being moved, and it must stay green through it.

## Events

`internal/agent` already publishes everything a UI needs through `agent.Events`
(`OnContent`, `OnToolBatch`, `OnAgentStart`, `OnTodoUpdate`, `OnUsage`, ...).
That callback struct is the existing UI contract; `uisession` serialises it and
adds the events that belong to the session rather than to a turn.

```go
type Event struct {
    Seq  uint64    `json:"seq"`
    Time time.Time `json:"time"`
    Kind Kind      `json:"kind"`
    // ... the fields each kind uses, omitted when they do not apply
}
```

It is a flat struct rather than a kind-tagged `Data json.RawMessage`, which is
what the design first called for. The same value is written to the journal,
handed to an in-process front-end, and decoded on the far side of a websocket;
a flat struct costs nothing in all three, while a nested payload would mean
marshalling every streamed delta just to hand it to a renderer in the same
process. `internal/trace` already stores events this way.

`Seq` is monotonic per session and assigned under the same lock that appends to
the journal, so the journal order and the subscriber order can never disagree.

| Kind                | Payload                                     | Source          |
| ------------------- | ------------------------------------------- | --------------- |
| `user_message`      | `text`, `images`                            | `uisession`     |
| `turn_start`        | -                                           | `uisession`     |
| `turn_end`          | `text`, `error`, `interrupted`              | `uisession`     |
| `content`           | `delta`                                     | `agent.Events`  |
| `reasoning`         | `delta`                                     | `agent.Events`  |
| `assistant_message` | `text`                                      | `agent.Events`  |
| `tool_batch`        | `round`, `calls[]{id,name}`                 | `agent.Events`  |
| `tool_start`        | `id`, `name`, `args`                        | `agent.Events`* |
| `tool_end`          | `id`, `name`, `text`, `bytes`, `blob`, `error` | `agent.Events`* |
| `error`             | `text`                                      | `uisession`     |
| `agent_start`       | `id`, `agent`, `prompt`                     | `agent.Events`  |
| `agent_end`         | `id`, `result`, `error`                     | `agent.Events`  |
| `sub_tool_start`    | `id`, `agent`, `tool`, `args`               | `agent.Events`  |
| `sub_tool_end`      | `id`, `agent`, `tool`, `result`, `error`    | `agent.Events`  |
| `sub_notice`        | `id`, `agent`, `text`                       | `agent.Events`  |
| `notice`            | `text`                                      | `agent.Events`  |
| `usage`             | `tokens`                                    | `agent.Events`  |
| `todo`              | `items[]`                                   | `agent.Events`  |
| `budget_exhausted`  | `reason`                                    | `agent.Events`  |
| `file_changed`      | `path`                                      | `uisession`     |
| `approval_request`  | see below                                   | `uisession`     |
| `approval_resolved` | see below                                   | `uisession`     |
| `session_meta`      | `id`, `title`, `model`, `ctx`               | `uisession`     |
| `presence`          | `clients[]{id,kind,label}`                  | `uisession`     |
| `desync`            | `from`                                      | fan-out         |

The `id` on `agent_*` and `sub_*` is the parent `task` tool call's id, which is
what keeps concurrent subagent runs correctly grouped no matter how their events
interleave. The web UI renders them as parallel lanes rather than as the
interleaved waterfall the terminal is stuck with.

### Prerequisite: call ids on tool events

*Done in step 0.* The two rows marked `*` could not be produced when this was
written. `OnToolStart` and `OnToolEnd`
carry only `(name, args)` and `(name, result, err)`; the per-call ids exist, but
only inside `OnToolBatch`, whose own comment notes that the per-call events
which follow "cannot tell a batch of three from three consecutive single calls".
`OnSubToolStart`/`OnSubToolEnd` have the same gap one level down: their id is
the parent `task` call, not the individual nested call.

The terminal gets away with this because it appends lines in arrival order. A
web UI cannot: it needs to attach a result to the card of the call that produced
it, and the blob store is keyed by call id. Correlating by arrival order is
wrong the moment a batch runs concurrently, which is the normal case.

So `internal/agent` gained the call id on those four callbacks. It is a small
change to `runToolCall`, which already had the id in hand, but it touched
`internal/trace` as well as the six call sites across `agent`, `tui`, `bot` and
`cmd/aigem` - traces now record which call produced which result, rather than
only the order the lines landed in.

### Approvals

The TUI currently carries the response channel inside the message:

```go
type confirmReqMsg struct {
    name, args string
    resp       chan bool
    ...
}
```

That works for exactly one in-process consumer. With several clients the channel
moves into a private table owned by `uisession`, keyed by an id that goes out in
the event:

```jsonc
// approval_request: the request is nested, and each option carries the label
// to show for it - "Always" means something different for a tool, for a read
// outside the root, and for a write outside it.
{"kind": "approval_request", "id": "ap-7", "approval": {
  "kind": "tool", "tool": "bash", "args": {...},
  "options": [{"value": "once", "label": "Once"},
              {"value": "always", "label": "Always"},
              {"value": "deny", "label": "Forbid"}]}}

// approval_request, path variant
{"kind": "approval_request", "id": "ap-8", "approval": {
  "kind": "path", "tool": "read_file", "path": "/etc/hosts", "write": false,
  "options": [{"value": "once", "label": "Once"},
              {"value": "always", "label": "Always (this folder)"},
              {"value": "deny", "label": "Deny"}]}}

// approval_resolved
{"kind": "approval_resolved", "id": "ap-7", "decision": "once", "by": "phone"}
```

A rejected client frame is not an event. It goes back as `{"kind":
"client_error", "op": ..., "error": ...}`, deliberately not `error` - naming it
that made every client mistake a refused request for something that happened in
the conversation, and put "approval already decided" into the timeline as a
failure at exactly the moment this design says it must not be one.

`Resolve` takes the lock, removes the entry, and sends on the parked channel.
A second `Resolve` for the same id finds nothing and returns `ErrAlreadyDecided`.
`approval_resolved` is broadcast to **every** subscriber, so the other clients
close their dialog showing who answered rather than reporting an error. That is
the whole of the race handling: first responder wins, everyone else is told.

The queue of approvals waiting behind the current one already exists in the TUI
and moves across unchanged.

`presence` matters more than it looks. An unanswered approval blocks the turn,
and without knowing whether anyone is watching, a client cannot tell "thinking"
from "waiting for a human who left".

## Journal

Sessions today are a flat `sessions/<id>.json` holding `Meta` plus the message
history. That is what the agent needs to resume; it is not enough to rebuild a
timeline, because it has no tool calls, no subagent structure and no artifacts.
Compaction also evicts messages the timeline should still show.

So the two are separated. The journal is a parallel tree rather than a
restructuring of the existing one:

```
sessions/<id>.json            Meta + []llm.Message   (agent state, unchanged)
journal/<id>/events.jsonl     one Event per line     (UI state)
journal/<id>/blobs/<seq>      the tail of a large tool result
```

The design first called for a directory per session holding both. Keeping them
apart turned out to be strictly better: `internal/session` is used by the bots
and by `cmd/aigem` as well as here, and nothing about the timeline requires
moving the file they all read. Existing sessions resume with no migration and
no back-compat branch, which is the cheapest possible answer to a requirement
that was only ever "the journal persists".

Sequence numbers restart at 1 in each process, so resuming a conversation picks
the sequence up after the journal's last entry. Appending without that would
write a second event under a number the file already uses, and a client asking
for everything after it would be served halves of two conversations.

`events.jsonl` is append-only and is the source of truth for any client catching
up. A large tool result is stored as its head, with the rest beside it in
`blobs/`, fetched when someone expands the call; live subscribers get the event
whole. Without that split, one `grep` over a large tree sits in the journal and
ships again on every reconnect.

The design assumed the untruncated body would be available to store. It is not:
`clipToolResult` runs inside the agent before the event is published, so what
reaches the session is already bounded. The "full" body in `blobs/` is therefore
the model-visible result, which is the thing worth inspecting anyway - the
inline head only has to be small enough that a reconnect stays cheap.

Blobs are keyed by event seq rather than by call id. A call id is only unique
within the agent that issued it: when a provider supplies none, `callRefs` falls
back to a counter that is per-`Agent`, so two concurrent subagents can both
produce `call-1`. Grouping in the UI is unambiguous because a nested call is
identified by its run id together with its call id, but a flat filename is not,
and the seq already is.

A session that cannot open a journal keeps working. The in-memory history still
serves a reconnect within the process, and losing the ability to replay across a
restart is not a reason to refuse to run.

## Websocket protocol

One connection per attached session.

```
GET /api/sessions/{id}/socket?since=<seq>&token=<token>
```

On connect the server replays `Replay(since)` and then switches to the live
tail, so a client that slept through a tunnel, a lift or a phone lock reconnects
with its last seq and misses nothing.

Client to server:

```jsonc
{"op": "submit",    "text": "...", "images": [...]}
{"op": "interrupt"}
{"op": "resolve",   "id": "ap_7", "decision": "once"}
{"op": "command",   "name": "compact", "args": ""}
```

Server to client: `Event`, verbatim.

**Backpressure.** A phone on a bad connection must not stall the agent. Each
subscriber has a bounded buffer; on overflow the server drops the subscriber
with a `desync` event carrying the last seq it definitely delivered, and closes.
The client reconnects with `since` and catches up from the journal. Blocking the
event fan-out on the slowest reader would let one stuck tab hang a turn.

The token travels in the query string because browsers cannot set headers on a
websocket handshake. On loopback that is acceptable; it is worth revisiting
before any `--listen` beyond `127.0.0.1`, since query strings leak into logs and
referrers.

## HTTP API

```
GET    /api/sessions                    list
POST   /api/sessions                    create {cwd, model, profile}
DELETE /api/sessions/{id}               close and archive
GET    /api/sessions/{id}/events?since= replay (non-websocket clients, debugging)
GET    /api/sessions/{id}/blobs/{seq}   the tail of a large tool result
GET    /api/sessions/{id}/artifacts     files this conversation changed
GET    /api/usage                       quota readings, per provider
GET    /api/models                      registry + which are authenticated
POST   /api/auth/login/{provider}       begin a login, returns a flow id
GET    /api/auth/login/{flow}           poll: pending / done / error
POST   /api/auth/login/{flow}/paste     redirect-URL fallback (see below)
GET    /healthz
```

HTTP uses `Authorization: Bearer <token>`.

### Provider login from the browser

The two flows in `internal/auth` behave differently once the browser is not on
the same machine as the daemon:

- **xAI (`LoginXAIDevice`)** is a device-code flow. The UI shows the URL and the
  user code, the daemon polls. Works from anywhere, including a phone.
- **ChatGPT (`LoginChatGPT`)** is authorization-code with a hardcoded
  `http://localhost:1455/auth/callback` redirect. From a laptop browser on the
  same machine this works unchanged. From a phone it cannot: the provider
  redirects the *phone's* browser to the phone's own localhost.

The fallback for that already exists. `startCallback` accepts a paste of the
redirected URL (`allowStdinPaste`), and in the browser that becomes a text field
instead of a stdin read. The work is decoupling `callbackServer` from the
assumption that stdin is a terminal, not inventing a new flow.

## Security

The daemon binds `127.0.0.1` by default. Loopback keeps the network out, but it
does not keep browsers out: any page in any open tab can issue requests to
`127.0.0.1`, and DNS rebinding defeats the same-origin policy. Behind this
endpoint sits an agent with `bash` and filesystem writes, and, once the login
flows above land, the credential store as well.

Therefore, from the first commit:

- **A token**, generated at startup and printed in the URL, in the style of
  `jupyter`. Checked on every HTTP request and on the websocket handshake.
- **`Origin` and `Host` allowlists** on the handshake, matched exactly rather
  than by prefix. No CORS headers are emitted at all.
- **A capability profile per session**, resolved at creation through the
  existing `tools.ResolveCapabilityProfile`. The daemon does not get a way to
  escalate a session mid-flight, in line with the existing rule that unattended
  paths never escalate.

Exposing the daemon beyond loopback is deliberately a separate, later decision.
The two candidates are `tailscale serve` in front of it, or `tsnet` inside it
for tailnet identity and TLS without configuration; the second costs a large
dependency and is not worth it until the rest works.

## Build and distribution

The front-end is React, Vite, TypeScript, Tailwind and shadcn/ui. Its sources
live in `internal/web/ui/` and take no part in a Go build.

```
make web    npm ci && npm run build  ->  internal/web/dist/
```

`internal/web/dist/` is committed containing only `.gitkeep`, so `//go:embed
all:dist` compiles whether or not the UI was built. At runtime the server checks
for `index.html` and, when it is absent, fails with a message rather than
serving a blank page:

```
$ aigem web run
aigem: this build has no web UI (built without `make web`).
Download a release binary, or run `make web && make build`.
```

This is a deliberate trade. `go install github.com/gigovich/aigem/cmd/aigem@latest`
is the first line of the README and must keep working on a machine with no
node toolchain, so the built assets are not committed. Release binaries carry
the UI because goreleaser runs `make web` in its `before` hooks.

## Work breakdown

Each step is meant to be one reviewable change that leaves `main` shippable.
The ordering is chosen so the risky part (step 2) is the only one that can
regress behaviour users already have, and so it lands against a test suite that
has not moved.

### 0. Call ids on tool events

`agent.Events` gains the call id on `OnToolStart`, `OnToolEnd`,
`OnSubToolStart`, `OnSubToolEnd`. `runToolCall` already holds it.

Touches `internal/agent/{agent,subagent,skilltool}.go`, `internal/bot/runtime.go`,
`internal/tui/tui.go`, `cmd/aigem/main.go`. Six call sites, no behaviour change.

*Done when* the suite is green and a new agent test asserts that a batch of
three concurrent calls produces start/end pairs that can be matched by id.

### 1. `internal/uisession`, standalone

The package with no consumers yet: `Event`, `Decision`, the `Session`
interface, and `Local` wrapping `agent` + `tools` + the retrying stream. The
approval table, `Subscribe`/`Replay` with an in-memory ring, and the
backpressure rule.

*Done when* unit tests cover the three things that are hard to get right and
impossible to test through a UI: two `Resolve` calls for one id (second gets
`ErrAlreadyDecided`, both clients see `approval_resolved`), a slow subscriber
being dropped with `desync` without stalling the others, and `Replay(n)`
splicing into the live tail with no gap and no duplicate seq.

### 2. Move the TUI onto it

The extraction. `New` gives up constructing the agent; `agentEvents` becomes a
subscriber loop; `confirmReqMsg` loses its `resp`/`pathResp` channels in favour
of an id. Moving out: `startTurn`, `submit`, `runSkill`, `runMcpPrompt`,
`runCompact`, `finishTurn`, `newSession`, `loadSession`, `saveSession`, the
confirm queue (`handleConfirmReq`, `answerConfirm`, `answerPathReq`,
`promoteNextConfirm`), the model machinery (`switchModel`, `runModelCommand`,
`selectLocal`, `startLocal`, `assessActiveModel`), `runLogin`/`doLogout`, and
`buildCommands` (the web palette needs the same catalogue).

Staying: everything that draws or reads keys. The `[]block` timeline stays too
and becomes a projection of the event stream rather than of `tea.Msg`.

This is the only step that can break something that works today, so it is worth
splitting into three commits: turns and approvals; sessions, models and login;
the command catalogue.

*Done when* `internal/tui` no longer imports `internal/agent` for control, only
for types, and every TUI test that asserts on *behaviour* passes unmodified.

The stricter form of that criterion - the whole suite unmodified - turns out to
be unachievable, and it is worth saying why rather than discovering it during
the change. Part of the suite asserts on the internals that are being moved:
`tui_test.go` builds `confirmReqMsg` values with a `resp chan bool` inside
(lines 1168, 1198, 1406), reads `m.pendingQueue`, and inspects `m.toolPolicy`
(lines 989, 1445). Those fields are precisely what moves, and the equivalent
assertions already exist against `uisession` (`TestResolveFirstAnswerWins`,
`TestQueuedRequestsSettledByPolicy`). So the rule for review is narrower: a test
that pokes at a relocated field may be deleted once the new package covers the
same ground, and a test that drives the Model and asserts on what it renders may
not change at all.

### 3. The journal on disk

Directory-per-session, `events.jsonl`, `blobs/`, and back-compat reading of the
flat `sessions/<id>.json`. `Replay` switches from the ring to the file.

*Done when* a client beyond the retained history is served from the journal
instead of being told to reload, an oversized tool result is stored as a head
plus a blob, and a resumed conversation continues the sequence rather than
reusing numbers.

Drawing the restored timeline is the other half, and it belongs to the
front-ends: `Timeline()` returns what was recorded, and a front-end decides what
to do with it. The terminal still rebuilds its history from the saved messages,
which is why it still shows no tool calls on resume; the browser reads the
timeline from the start, and the terminal can follow once there is something to
compare it against.

### 4. The daemon, without a UI

`internal/web`: session manager, `web.json`, HTTP routes, the websocket, the
token and the `Origin`/`Host` checks. No frontend assets involved.

*Done when* a Go test drives a full turn over the websocket - submit, tool call,
approval, answer - and a second connection with `since` gets an identical
timeline. Also when a request with a bad `Origin` or no token is rejected, which
is a test worth writing before the UI makes it easy to forget.

One conversation at a time to begin with, lifted in step 8. Sessions built
against a single tool registry are not independent: registering the delegation
tool binds it to the confirmation function of whichever session registered it
last, so a tool call in one conversation would ask another conversation's
clients for approval. `MaxSessions` still exists for a caller that wants a cap;
it is a choice now rather than the only safe setting.

Three things a websocket needs that an HTTP handler does not. The connection is
hijacked, so the http server can no longer close it: when the session ends
first, the write side has to close the connection or the read side sits on a
read that never returns and shutdown hangs. And closing has to be serialised
with writing, or a close partway through a frame leaves the client reading the
tail of one frame as the header of the next - which it reports as a reserved
opcode, a confusing way to learn about a race.

And a client must read from the buffer its handshake left behind, not from the
connection: a dialer reads ahead, so frames the server sent right after the
upgrade are already in that buffer. Reading past it loses the start of the
stream and picks it up mid-frame. This bit the daemon's own tests before it
could bite a front-end, which is the useful order for it to happen in.

### 5. Frontend scaffolding

Vite, React, TypeScript, Tailwind, shadcn/ui in `internal/web/ui/`. `make web`,
the `go:embed`, the `.gitkeep`, the `.gitignore` entry, the goreleaser hook, and
the error message for a build without assets.

*Done when* `make web && make build && aigem web run` serves a page that streams
one turn, and a plain `go build` still produces a working binary that refuses
`web run` with the intended message.

The embedding, the `.gitkeep`, the `make web` target, the goreleaser hook and
the message for a build with no assets landed with step 4, so this step is the
frontend itself.

### 6. Web UI v1

Timeline, streaming answer, tool call cards with lazy blob expansion, subagent
lanes, the approval dialog, rendered markdown, interrupt, todo panel, context
and spend in the header.

Two things the browser does that the terminal cannot, and they are the reason
the step exists rather than being polish. A tool call and its result are one
card, because the result is attached by id rather than by whatever line arrived
next. And a delegated run is its own lane, because a terminal has one column and
concurrent subagents have to interleave in it.

Markdown from a model is untrusted input rendered as HTML, so it is sanitised.
Links are given `target=_blank` and `rel=noopener` after sanitising, since the
sanitiser strips them.

The token arrives in the URL and is kept in `sessionStorage`, not
`localStorage`: it authorises an agent with a shell, and it should not outlive
the tab it was handed to.

### 7. `uisession.Remote` and `aigem attach`

The second implementation of the interface, over the same protocol.

*Done when* a TUI and a browser on one session both see every event, and an
approval answered in one is reported as resolved in the other.

The interface had to be settled first, and settling it drew a line that was not
obvious from the outside. What crosses is what a front-end *does* to a
conversation - submit, interrupt, resolve, command - plus the two things it
cannot learn any other way: the conversation's identity, and the approval
already blocking it when the client arrived. Everything else a front-end used to
ask for is now something it is told: the context window, the token count and the
title arrive as events, so a remote client is not a chain of round trips over a
conversation it is already streaming.

What does not cross is running a turn from a closure. A skill, an MCP prompt and
a compaction all hand the session a function that drives the agent, and a
function does not travel; those reach a remote session as commands instead.

The model is written against the interface for the conversation and holds the
local session separately for what cannot cross. Submitting a message - which is
almost all of the use - goes through the interface; a turn driven by a closure
goes through the local session and says so when there is none. The gauge's
denominator moved onto the stream with the rest of the conversation's identity,
so it works the same either side of the wire.

`aigem attach` is still a stream client rather than the Bubble Tea model. What
is left is not the model's dependencies any more but its construction: `tui.New`
builds an agent, a registry, skills and MCP because a local session needs them,
and an attached one needs none of it. That is a second constructor and a set of
absent-collaborator paths, and it is worth doing next to the multi-session work
rather than before it.

### 8. Multi-session and mobile

A sandbox per conversation, session list and switching, `presence`, reconnect on
`desync`, and an installable page. This is the step at which the phone scenario
works end to end.

The registry is the whole of it. Everything else - the list, the switcher, the
reconnect - was already there or is a dozen lines; what blocked several
conversations was that they would have shared one, and a shared registry hands
the delegation and skill tools whichever confirmation function was registered
last. The guard for that lives against the factory the command builds, not the
daemon's, because the daemon's own tests inject their own.

What is still per-root rather than per-session: skills, project instructions and
trust, all resolved once at startup. That is why a session asked for another
directory is refused rather than sandboxed to the wrong one.

No service worker. The page is useless without the daemon, and an offline cache
would only ever show a conversation that has moved on; the manifest is there so
a phone can keep it on the home screen, which is the part that was actually
wanted.

### 9. Login in the browser

Device code for xAI; the redirect-URL paste fallback for ChatGPT, which meant
decoupling `callbackServer` from stdin.

The flows are the same ones the terminal runs, split so the blocking half can be
watched instead of waited on: `auth.Begin` does the part that produces something
to show - the device code, or the authorize URL and a bound callback - and
returns, leaving the wait on its own goroutine. Nothing about the CSRF rule
changes: a callback arriving over HTTP must echo the state exactly, and a pasted
URL bearing one must match it too.

`Paste` is refused for a flow with no callback waiting, so a device login cannot
be fed an authorization code from somewhere else, and a second answer cannot
displace one that already arrived.

The API reports where to send the user and whether it worked. It never returns a
token, a code or an exchange - a client does not need any of that, and the
credential store is exactly what the token and origin checks were put there to
protect.

### 10. Web-only surface

Side-by-side diffs and the spend readings have landed. The file tree and the bot
console have not.

Both of the built ones are read-only views over state the agent already keeps,
which is why neither needed a new event: the session has tracked what it changed
since step 1, and the quota readings have been persisted from real responses
since long before any of this.

The diff list carries filenames only, and content arrives when a path is named.
The list is opened far more often than any one diff is read, and a session that
rewrote a large tree would otherwise ship all of it to draw a sidebar. The
"before" is the file as it was when the session first touched it, not before the
last edit, because the question someone reviewing a run asks is what the run
did.

Per-hunk revert is not built. Reverting is a write, and a write from the browser
is a different thing from a view of one: it wants the same approval path a tool
write goes through, or it is a way to change files that skips the trust model
entirely. That is a design question, not an afternoon of UI.

The file tree needs a listing endpoint over the sandbox, and the bot console
needs `internal/bot` to expose fleet state that today only its logs carry.
Neither is blocked; both are their own piece of work.

## Risks

- **Step 2 is the whole bet.** Roughly 1500 lines of logic move between packages
  in one go. The mitigation is that the TUI suite is large, predates the change
  and does not move with it.
- **`glamour` is a terminal renderer.** The TUI's markdown pipeline does not
  cross over; the web renders markdown itself. Nothing is shared there, and
  trying to share it would be a mistake.
- **Two front-ends, one feature.** The stated intent is that features land in
  the web first and may never reach the TUI. That is fine as long as they land
  in `uisession` rather than in `internal/web`, or the split rots from the other
  side.

## Open questions

- **Session lifetime.** When does a detached session stop, and what garbage
  collects `events.jsonl` and `blobs/`? A daemon left running for a week with no
  policy will accumulate both.
- **Concurrent turns.** One turn at a time per session is assumed. Nothing in
  the protocol prevents a second `submit` arriving mid-turn; today `Inject`
  exists for exactly that. Which of the two applies should be decided before the
  UI implies an answer.
- **Notifications.** Web Push needs HTTPS, which loopback does not provide. An
  approval waiting on a phone is the most valuable notification in the system,
  and it is blocked behind the `--listen` decision.
- **Upload limits** for pasted images, and where they are stored.
