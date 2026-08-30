# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `aigem web` serves a browser UI on a loopback port. It is the first phase of
  the rewrite promised below: the daemon, the app shell and the build
  integration, with the screens still to come. The printed URL carries a token;
  the page trades it for an `HttpOnly; SameSite=Strict` cookie and takes it back
  out of the address bar. Browser sign-ins survive a restart in
  `$XDG_STATE_HOME/aigem/web-cookies.json`.
- `aigem web --origin https://name.example.ts.net` states the public URL the
  daemon is reached at, which is what lets it bind an address the network can
  reach. Without it the bind is refused: an origin check needs a name a person
  stated, and nothing in a request can be trusted to supply one. Repeat the flag
  for a daemon reached under more than one name; give an internationalised name
  in its punycode form. A loopback bind behind `tailscale serve` or another
  reverse proxy still needs no flag at all.
- `aigem web --sign-out` forgets every browser session before serving. Sign-ins
  outlive a restart on purpose, so a restart mints a new token and leaves every
  cookie working - which makes it the wrong reflex for a token that got out.
  Stop the daemon first: a running one holds the sessions in memory and goes on
  honouring them.
- `make web` builds the browser UI into `internal/web/dist`, where the binary
  embeds it from. It needs Node 22+. **The bundle is not committed and the
  release pipeline does not build it**, so a downloaded release binary and
  `go install ...@latest` both answer every page with a 501 that says which
  build step is missing. Build from a checkout to get a UI.

### Fixed

- The `bash` tool no longer waits for a command's backgrounded children after
  its context is cancelled. Killing the shell left an orphan holding the output
  pipe, so an interrupted turn ran on for as long as that child lived - thirty
  seconds for a `sleep 30 &`, and unbounded for anything long-running. It gets
  the process group and the `WaitDelay` that hooks have had.
- A session that was closed while a turn was running could still be writing
  after `Close` returned. The turn is emitted as ended and saved after that, so
  a caller that closed on seeing the end of it raced a write it had no way to
  know about. `Close` now waits for the turn to unwind. This was visible as an
  occasional `TempDir RemoveAll cleanup: directory not empty` under `-race`.

### Changed

- Saved conversations are written atomically and 0600, through the new
  `internal/store`, in a 0700 `sessions` directory. They used to be written
  0644 with `os.WriteFile`, which truncates the file before writing it: anyone
  reading the same conversation in that window - soon a browser tab, or a
  second aigem - saw a document that was not JSON, and a process killed there
  left one on disk. Files already written keep their mode until they are saved
  again.
- A session id that is a path rather than a name is refused, and a document
  whose own id disagrees with the file it was found in is refused with it. Ids
  come from `NewID` today, but the browser daemon takes them from requests,
  `../auth` was a path the sessions directory had no reason to reach, and the
  id inside the document is the one callers carry away to name the event
  journal.

### Removed

- The bot/fleet subsystem and the legacy browser Web UI. `aigem bot ...`,
  `aigem chat ...`, `aigem web run`, `aigem attach`, and the global `-listen` /
  `-origin` flags are gone, along with bot accounts, roles, bot memory,
  cron/scheduling, heartbeats, handoffs, team status, the fleet's shared
  conversation store, the Web UI and its HTTP/websocket API and Web Push
  notifications, the Docker image and `docker-entrypoint.sh`, the `deploy/`
  systemd units, and Node/npm as a build prerequisite for the binary. The
  replacement Web UI is being designed from scratch and its first phase is in
  the same release - see Added above; nothing from the old one is carried
  over. Nothing under `~/.config/aigem/bots/`, `~/.config/aigem/fleet.json`,
  or the fleet/web daemon's state files is deleted automatically - remove them
  by hand if you want the disk space back. Coding subagents (`task`, the
  scout/code-writer/simplifier/reviewer delegation the model uses mid-turn) are
  a separate system and are unaffected.

## [0.4.0] - 2026-08-26

### Added

- The browser Fleet screen can change a bot's saved model or return it to the
  role default. It shows configured intent, selected source, the model already
  running, and a restart-required state separately; a save is visible only after
  daemon validation and persistence succeed. Options come from the same trusted
  user registry as `aigem bot model`, unavailable authentication states are
  explained, and the write API exists only in the bot-owning daemon rather than
  standalone web mode.

- Browser sessions survive a restart. Each daemon keeps its cookie table in a
  file of its own - `chat-cookies.json` for the fleet, `web-cookies.json` for
  `aigem web run`, both 0600 in `$XDG_STATE_HOME/aigem` - so deploying a new
  binary no longer signs out a phone whose only way back in is a token on
  another machine. Expired entries are dropped on load, an unreadable file
  costs the sessions and nothing else, and revoking is logging out or deleting
  the file while the daemon is stopped.

- Web Push: the fleet can now reach a phone with nothing open. The daemon
  generates a VAPID key pair once into `$XDG_STATE_HOME/aigem/chat/vapid.json`,
  a page that has been granted the notification permission subscribes to it, and
  one event is pushed - a thread turning to `needs you`. RFC 8291 encryption is
  written against the RFC's own test vector and RFC 8292 signing against a token
  the tests verify, rather than either being taken as a dependency. It needs
  HTTPS, so it follows the reverse proxy; on iOS it needs the page installed to
  the home screen. A daemon whose keys cannot be loaded serves the fleet as
  before and says so in the log.

### Fixed

- Notifications from an open page no longer depend on a constructor Chrome for
  Android refuses. They go through the service worker's registration when there
  is one, which is every page that has subscribed to push.

- A bot that failed to start no longer shows as running. `present` was set for
  every configured bot before any of them had started and never cleared, so a bot
  whose model could not be opened still drew a running dot in the inbox, the
  composer and every participant list. It is written when a bot comes up and
  cleared when it goes down.

- An "@name" is a mention only at a word boundary. The pattern had no leading
  boundary, so "mail me at someone@demetre" named demetre and woke them. The
  composer's autocomplete decides whom to add to a thread and the daemon decides
  whom a message names, so the two now parse by the same rule - a name only one
  of them recognised was a bot that joined without being addressed, or was
  addressed without joining.

- Bots can search their own threads again. `read_chat` was renamed `read_threads`
  when the fleet moved off Mattermost, but the capability profile every bot's
  toolset is intersected with kept the old name - so the tool was filtered out of
  every bot, silently, with nothing logged and no error to notice. A role list
  and a profile that disagree now fail a test rather than a bot.

- Saving the input-history file no longer happens on the render loop. It takes a
  cross-process lock and two fsyncs, so a second aigem open on the same directory
  could freeze this one for the lock timeout - no redraw, no keys - and then
  print the timeout into the chat. Recall updates immediately and the write is a
  background command.

- A history file that cannot be read back is discarded and rebuilt. The same read
  runs inside the write path, so a corrupt or oversized one used to disable
  saving for good, put an error in the chat on every prompt, and - by leaving a
  block behind at startup - displace the welcome screen permanently.

- Scrolling up to read history survives typing. Every relayout - a row gained by
  the input box, an arrow key inside an overlay - rebuilt the chat viewport, and
  a fresh one reads as "at the bottom", so the next redraw jumped to the end.
  The viewport is resized in place now.

- Scrolling the chat fast no longer types escape sequences into the input box.
  A quick wheel spin arrives as one dense burst of SGR mouse reports, and Bubble
  Tea v1 read stdin in 256-byte chunks: whenever a report straddled that
  boundary, its tail was handed on as literal text and the textarea typed it.

- A transient provider error during a delegated subagent or a forked skill is
  retried again. The retry decorator refuses to re-issue a stream that already
  emitted text, so the caller is not shown the same deltas twice - but a nested
  run's deltas are shown nowhere, and its partial answer is discarded when the
  run fails, so the rule protected nothing there and turned a 5xx into a failed
  delegation. On a reasoning model, whose deltas start immediately, that was
  nearly every hiccup.

### Fixed

- The `bash` tool no longer waits for a command's backgrounded children after
  its context is cancelled. Killing the shell left an orphan holding the output
  pipe, so an interrupted turn ran on for as long as that child lived - thirty
  seconds for a `sleep 30 &`, and unbounded for anything long-running. It gets
  the process group and the `WaitDelay` that hooks have had.
- A session that was closed while a turn was running could still be writing
  after `Close` returned. The turn is emitted as ended and saved after that, so
  a caller that closed on seeing the end of it raced a write it had no way to
  know about. `Close` now waits for the turn to unwind. This was visible as an
  occasional `TempDir RemoveAll cleanup: directory not empty` under `-race`.

### Changed

- `aigem chat read --json` prints a paging envelope rather than a bare array:
  `{"items": [...]}`, plus `cursor` and `more` when older messages remain. A
  bare array that stopped at `--limit` was indistinguishable from the start of
  the conversation, so a script reading a long thread silently saw part of it
  and could not tell. The same envelope is on `GET /api/chat/threads/{id}/
  messages`, `/timeline` and `/turns`. Scripts that consumed the array need
  `.items`.

- Input history records prompts only: slash commands are skipped, since `/new` is
  the last thing most sessions see and would be the first thing Up offered, and
  so are empty submissions and anything past 16 KiB. Abandoned temp files are
  swept, and the directory and lock file are opened without following symlinks.

- The TUI moved from Bubble Tea, Bubbles and Lip Gloss v1 to v2
  (`charm.land/*/v2`), which is where the input parser above is fixed. Key
  events now carry a code plus modifiers instead of a type per combination, and
  mouse events are one message type per action. Glamour still renders markdown
  through Lip Gloss v1, so both major versions are in the build.

- The delegation guidance moved out of the built-in system prompt and is now
  appended alongside the skill, search and MCP blocks. It used to vanish behind a
  custom `~/.config/aigem/SYSTEM.md` while the `task` tool stayed registered,
  which left the model a capability nothing explained. It is also built from the
  agent registry now, so custom subagents appear in it instead of the four
  built-in names being hardcoded, and it says outright when NOT to delegate -
  the missing half of the advice, since a second context costs more than reading
  one file.

### Removed

- Mattermost. The bot fleet no longer talks to it, and the ~2600-line REST and
  websocket client is gone. It was the transport and the history store at once,
  which meant the authorisation boundary - channel membership - was a question
  asked of an external server with the bot's own credentials, and answered by
  falling back to a guess when it could not be confirmed. Participation in a
  thread replaces it: a bot reads and writes exactly the threads it is in, there
  is no second authority to disagree with, and a refusal is final.

  `bot create` is four questions and no network call; the `transport:` block in
  an existing `bot.yaml` is ignored rather than rejected. Bot tokens in the auth
  store go with `aigem bot rm`, and tokens already issued to a Mattermost server
  should be revoked there. Nothing in aigem reads them any more, so an existing
  server can stay up read-only for as long as its old threads are wanted.

- The `aigem-bot@.service` unit. Its `Conflicts=aigem-bots.service` line existed
  only because one Mattermost account allows one websocket; with the fleet on
  its own store, two units means two SQLite writers serving two copies of the UI
  on two ports. `superviseBot` already restarts an individual bot inside the one
  process, and `aigem bot start <name>` is the way to debug one in the
  foreground.

### Added

- The bot fleet has its own conversation store and its own browser screen. A
  thread is one task with an explicit set of participants - the operator and one
  or more bots - kept in SQLite at `$XDG_STATE_HOME/aigem/chat/chat.db`. There
  are no channels and no rooms: posting into a thread wakes everyone in it, and
  that is the whole wake mechanism, so the class of failure where a mention
  reached a bot that was not in the channel is gone.

  `aigem bot start` serves it, and `aigem chat threads|new|send|read|search|tail|
  fleet` drives it from a terminal. In a browser the same daemon opens a Bots
  screen beside the existing session workspace - an inbox sorted by what needs
  the operator, the thread itself, and a composer - with the screen switch
  appearing only on a daemon that serves both. Under each bot's answer is a one
  line summary of the run that produced it - `14 steps - 6 tools - 2 files` -
  which expands into the whole timeline: every tool call, its result, and the
  nested runs of any subagent it delegated to. Beside the thread are that run's
  changed files with their diffs, the bot's working plan, and what the thread
  has cost. This is the thing a chat product could never show, and the reason
  for the move.

  Bot conversation history was never persisted, so there is nothing to migrate:
  a restart already cost the fleet its in-memory context, and switching backends
  costs it exactly that much.

  A fleet screen goes with it, in the browser and as `aigem chat fleet`: which
  bots are up, what each is working on, how many threads it carries, the model it
  actually opened, how far its heartbeat has backed off, and what it is next due
  to do. Half of that comes from the store, so it agrees with the inbox and
  survives a restart; the other half only the running process knows, and a daemon
  that is not running the bots says so rather than reporting a stopped fleet. It
  includes the state that previously meant reading `journalctl`: a bot the daemon
  could not start and is still retrying.

- The daemon can be reached from another device. `--origin` names the public URL
  a reverse proxy serves it at, and the daemon refuses to bind anything but
  loopback without one - the allowlist it used to derive from its own bind
  address could never match a request arriving through a proxy, so the only
  possible answer was a 403 that read as a broken server. Configured origins
  replace that derived list rather than extend it, and are matched whole,
  scheme included; forwarded headers are not read at all.

  The token in the URL is now traded once for an `HttpOnly; SameSite=Strict`
  cookie, so it stops appearing in websocket URLs and in every access log on the
  way; it is `Secure` wherever TLS is involved, and lives in the daemon's memory,
  so restarting the fleet signs every browser out. The bearer token stays for
  `aigem chat` and `aigem attach`. Ten failed authentications a minute from one
  address now buy a 429, and thirty-two is the ceiling on concurrent websockets -
  an open tab holds two, so that is sixteen tabs across every device.

- A bot thread now records what the work in it cost. A model call made while a
  bot is working in a thread is billed to that turn - including the calls its
  subagents and its compaction make - and `aigem chat read` closes a transcript
  with the thread's tokens, calls and models. A call made outside a thread, on a
  heartbeat or a scheduled job, has no turn to belong to and still appears only
  in the log. Until now all of it did, attributed to a process rather than to
  any particular piece of work - and one client serves every thread a bot has
  open at once, so no total sampled around a turn could have been attributed
  correctly.
- `--trace-json` records a `-p` run's tool and delegation activity as JSONL. The
  human-readable stderr output was the only machine-adjacent record, and parsing
  it back is guesswork: it truncates, and it cannot show which calls the model
  emitted together in one assistant message - the difference between three
  subagents running in parallel and three running one after another. Each batch
  carries its calls' ids, so a nested run can be traced back to the call that
  started it: a delegated subagent and a forked skill announce themselves
  identically, and only the id tells them apart.
- A delegation eval harness in `evals/`. Fixture workspaces, scenarios that
  declare whether delegating is required, forbidden, or optional, and a runner
  that reports delegation recall and precision, agent-type accuracy, and parallel
  compliance over repeated runs. The unit tests prove the `task` tool works; this
  measures whether the model chooses to use it, which is a property of the prompt
  and was previously untested in any form. Every scenario must also assert that
  the work happened, or "did not over-delegate" would be a score a model wins by
  doing nothing.

## [0.3.0] - 2026-08-04

### Added

- `aigem bot start` now takes any number of bot names, and none at all. With no
  name it runs every configured bot in one process; with names it runs exactly
  those. Bots start one at a time, each connected before the next begins, because
  one Mattermost account allows one websocket.
- A fleet-wide cap on concurrent agent turns. Per-bot limits multiplied - five
  bots at four threads each aimed twenty conversations at one provider account -
  and nothing bounded the total. Scheduled runs take a slot too. A run of exactly
  one bot skips the turn cap: it has nobody to contend with, and a cap it could
  hit alone would only slow it down. One bot per unit (`aigem-bot@.service`) is
  therefore several processes that do not share any of these caps.
- A cap on how many browsers run at once across the process. Chrome is spawned
  per tool call and closed after, so this bounds a peak rather than a resident
  cost. Profiles stay per bot: they hold logins, and a shared profile would put
  every search behind one browser.
- `~/.config/aigem/fleet.json` sets both caps (`max_concurrent_turns`,
  `max_concurrent_browsers`). Omitting a key, or setting it to `0`, means the
  default - 6 turns and 2 browsers - and only a negative value means no cap.
- systemd units under `deploy/systemd/`: `aigem-bots.service` for the whole fleet
  in one process, and `aigem-bot@.service` for one unit per bot. The template
  declares `Conflicts=aigem-bots.service`, which systemd applies both ways, so
  the two cannot run together and open two websockets for one account.
- `team_status`, a tool listing the teammates in the process and whether each is
  working. A teammate that is mid-turn used to be indistinguishable from one that
  never got the message, which is what made bots ping each other repeatedly.
- A handoff or direct message to a teammate in the same process is now delivered
  in-process as well as posted to chat, so it survives a websocket that is down
  or reconnecting. Both copies carry the same post id and the teammate acts on
  whichever arrives first. Delivery stays inside the boundary chat enforces: the
  recipient confirms, with its own credentials, that it belongs to the channel
  the message came from, and teammates are matched by chat username rather than
  by aigem name.

### Fixed

- The `bash` tool no longer waits for a command's backgrounded children after
  its context is cancelled. Killing the shell left an orphan holding the output
  pipe, so an interrupted turn ran on for as long as that child lived - thirty
  seconds for a `sleep 30 &`, and unbounded for anything long-running. It gets
  the process group and the `WaitDelay` that hooks have had.
- A session that was closed while a turn was running could still be writing
  after `Close` returned. The turn is emitted as ended and saved after that, so
  a caller that closed on seeing the end of it raced a write it had no way to
  know about. `Close` now waits for the turn to unwind. This was visible as an
  occasional `TempDir RemoveAll cleanup: directory not empty` under `-race`.

### Changed

- Every LLM client in a process, the TUI's included, shares one HTTP connection
  pool, and bots share one OAuth token source per provider. Separate processes
  each refreshed the same token, which single-use refresh tokens do not tolerate.
- `aigem auth login` and `aigem auth logout` now drop the cached token sources,
  so a re-login takes effect without restarting the process.
- Every log line from a bot now carries its name.
- A bot keeps at most 200 per-thread agents, evicting the coldest idle one. The
  map was unbounded, which used to cost one process; with every bot's agents in
  one heap it would cost the whole team.
- A panic no longer ends the process. In a turn or a scheduled run it is logged
  and that one run is abandoned; the bot keeps serving. In a transport goroutine
  it ends that bot's event stream, which its supervisor treats as a stop.
- Each bot is supervised in-process and restarted when its stream ends, backing
  off from 5s to 5min and resetting only once a bot has stayed up a minute. A bot
  that cannot start is retried rather than taking the team down with it, so a
  permanently broken bot no longer ends a multi-bot process.
- The shipped system prompts are about a fifth shorter and rewritten in plainer
  English. Every behavioural rule is unchanged; the cuts are repetition, nested
  clauses, and rationale that restated its own rule.

### Fixed

- A bot under systemd could not use an SSH key its owner had loaded, and reported
  "the agent has no identities" for a key `ssh-add -l` showed in their terminal.
  The unit inherits `SSH_AUTH_SOCK` from the systemd user manager, which on a
  desktop names gnome-keyring's agent while the keys are in the OpenSSH one. Both
  units now carry a commented `Environment=SSH_AUTH_SOCK=` line for each of the
  two common sockets, since which one holds the keys is per-machine.

## [0.2.0] - 2026-07-31

### Fixed

- Path grants were unreliable on macOS. The store resolved symlinks only for
  paths that already existed and fell back to the raw path otherwise, so one
  directory could be recorded under two spellings. Where `/var` is a symlink to
  `/private/var` - the macOS default - an approved directory could silently stop
  being covered, and overlapping grants failed to collapse. The corrected
  resolution is now shared with the sandbox instead of duplicated.
- A provider declared `"auth": "none"` in `models.json` - a self-hosted
  OpenAI-compatible endpoint such as Ollama, vLLM, or a second llama.cpp - was
  reported as unauthenticated by the TUI. That put a modal alert in front of a
  model that works and pointed at a login that would have failed. Only the
  built-in local provider was exempt; `-p` was never affected.
- `aigem --help` printed a literal `%%` in the two compaction percentage flags.

### Fixed

- The `bash` tool no longer waits for a command's backgrounded children after
  its context is cancelled. Killing the shell left an orphan holding the output
  pipe, so an interrupted turn ran on for as long as that child lived - thirty
  seconds for a `sleep 30 &`, and unbounded for anything long-running. It gets
  the process group and the `WaitDelay` that hooks have had.
- A session that was closed while a turn was running could still be writing
  after `Close` returned. The turn is emitted as ended and saved after that, so
  a caller that closed on seeing the end of it raced a write it had no way to
  know about. `Close` now waits for the turn to unwind. This was visible as an
  occasional `TempDir RemoveAll cleanup: directory not empty` under `-race`.

### Changed

- An unknown subcommand now prints usage and exits non-zero. Previously anything
  unrecognized fell through and opened an interactive session in the current
  directory, so `aigem help` or a typo silently started a TUI. `aigem help` is
  now a real command.

### Added

- A demo recording in the README and on the docs home page, generated by
  `demo/record.sh` rather than captured by hand so it can be refreshed when the
  UI changes. The TUI, tools, sandbox, and markdown renderer in it are real;
  only the model is scripted, so re-recording needs no credentials.
- A published documentation site at
  [gigovich.github.io/aigem](https://gigovich.github.io/aigem/).

### Dependencies

- `chromedp` 0.16.0, `modelcontextprotocol/go-sdk` 1.7.0, `x/oauth2` 0.36.0,
  `charmbracelet/x/ansi` 0.11.7, alpine 3.24, and the GitHub Actions used by the
  release and docs workflows.

## [0.1.0] - 2026-07-31

First public release.

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
- Fixed a second escape of the same kind, via a *dangling* symlink.
  `EvalSymlinks` reports ENOENT both for a name that is absent and for a symlink
  whose target does not exist, so a link like `config.yaml -> /outside/target`
  was treated as a missing name and `write_file` created the target outside the
  sandbox - arbitrary file creation from nothing more than a `git clone`.
  Dangling links are now followed explicitly, with a hop limit for cycles.
- "Always (this folder)" no longer over-grants. Approving a *directory* recorded
  its parent, so allowing `/srv/data` silently granted `/srv` and every sibling,
  and approving a path that did not exist recorded its parent too - letting an
  invented path farm a grant over a real directory. The grant is now the folder
  the confirmation box named, and nothing is recorded for a path that does not
  exist.
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

### Fixed

- The `bash` tool no longer waits for a command's backgrounded children after
  its context is cancelled. Killing the shell left an orphan holding the output
  pipe, so an interrupted turn ran on for as long as that child lived - thirty
  seconds for a `sleep 30 &`, and unbounded for anything long-running. It gets
  the process group and the `WaitDelay` that hooks have had.
- A session that was closed while a turn was running could still be writing
  after `Close` returned. The turn is emitted as ended and saved after that, so
  a caller that closed on seeing the end of it raced a write it had no way to
  know about. `Close` now waits for the turn to unwind. This was visible as an
  occasional `TempDir RemoveAll cleanup: directory not empty` under `-race`.

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

[Unreleased]: https://github.com/gigovich/aigem/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/gigovich/aigem/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/gigovich/aigem/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gigovich/aigem/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/gigovich/aigem/releases/tag/v0.1.0
