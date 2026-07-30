# Docker (bot mode)

A small image for running a single bot non-interactively as `aigem bot start <name>`. The
bot's config and state are bind-mounted, so the image carries only the binary plus the
handful of runtime packages listed below.

Files live at the repo root: `Dockerfile`, `docker-entrypoint.sh`, `.dockerignore`.

## Pull or build

Every tagged release publishes a multi-arch image (amd64 and arm64):

```
docker pull ghcr.io/gigovich/aigem:latest
```

Or build it yourself. The Dockerfile uses BuildKit features - `$BUILDPLATFORM`
and `RUN --mount=type=cache` - so it needs `buildx`, the default on current
Docker. The legacy builder expands `$BUILDPLATFORM` to an empty string and fails.

```
docker buildx build --load -t aigem-bot .
```

`--load` puts the result in the local image store. Without it, a `docker-container`
builder keeps the image in its own cache and `docker run aigem-bot` reports that
the image cannot be found.

Multi-stage: `golang:1.26-alpine` compiles a static, CGO-free binary; the runtime is
`alpine:3.23` with `ca-certificates`, `bash`, `git`, `tzdata`, and `tini`. `tini` is PID 1 -
it reaps the subprocesses the `bash` tool spawns and forwards `SIGTERM` for a clean stop.

## Volumes

aigem namespaces its data under an `aigem` subdir of the XDG base dirs, and the image sets
`XDG_CONFIG_HOME=/config` and `XDG_STATE_HOME=/state`. So the data lives at **`/config/aigem`**
and **`/state/aigem`** - bind-mount the host dirs onto those paths (mind the `aigem` suffix):

- config -> `/config/aigem`: host `~/.config/aigem` (macOS:
  `~/Library/Application Support/aigem`). Holds `bots/<name>/bot.yaml` and each bot's memory.
- state -> `/state/aigem`: host `~/.local/state/aigem`. Holds `auth.json` (tokens) and sessions.

A `developer`/`tester` bot's workdir (`.`) is the container's `WORKDIR /workspace`; bind-mount
the repo it edits there.

The image ships `bash` and `git`, but a bot only reaches them if its
`capabilityProfile` is `shell` or `dangerous-shell`. The default
`workspace-write` has no `bash` at all - the role does not grant shell.

## Run

One bot per container; select it with `BOT_NAME`:

```
docker run -d --name aigem-jane \
  -e BOT_NAME=jane \
  -v ~/.config/aigem:/config/aigem \
  -v ~/.local/state/aigem:/state/aigem \
  -v /path/to/repo:/workspace \
  aigem-bot
```

An explicit command is passed straight to `aigem` and takes precedence over
`BOT_NAME`, so the image doubles as the CLI:
`docker run --rm -v ~/.config/aigem:/config/aigem aigem-bot bot list`. With
neither a command nor `BOT_NAME`, the container prints a hint and exits non-zero.

Do not run the same bot in two places at once (e.g. locally and in a container): two websocket
connections for one Mattermost account cause duplicate replies and authentication errors.

## Copying config and state to a server

`auth.json` holds tokens, so move it over SSH only. Skip runtime junk (`sessions/`,
`llama-server.*`, `browser-profile/`, and the host-specific `local.json`).

```
ssh user@server 'mkdir -p /srv/aigem/config /srv/aigem/state'

# config: bots/<name>/bot.yaml + memory, models.json, skills, agents
rsync -avz "$HOME/.config/aigem/" user@server:/srv/aigem/config/

# state: auth.json (+ search.json), minus runtime junk
rsync -avz --exclude 'sessions/' --exclude 'llama-server.*' \
  --exclude 'browser-profile/' --exclude 'local.json' \
  "$HOME/.local/state/aigem/" user@server:/srv/aigem/state/

ssh user@server 'chmod 700 /srv/aigem/state && chmod 600 /srv/aigem/state/auth.json'
```

The trailing `/` on each source copies the directory contents, so `bots/` lands directly in
`/srv/aigem/config/`. On macOS the config source is `"$HOME/Library/Application Support/aigem/"`.
Then mount `/srv/aigem/config:/config/aigem` and `/srv/aigem/state:/state/aigem`.

Tokens are tied to the Mattermost/OpenAI accounts, not the machine, so no re-login is needed
after the copy.

## Left out by design

- **No Go (or other language) toolchain.** Even with a shell profile, a bot cannot
  `go build` its project. Add the toolchain in a derived image or mount it.
- **No browser.** `web_search` works through the Brave Search API (key in state); the browser
  search backend and its `open_url` / `browser_action` tools do not.
- **No local llama.cpp.** The image targets a remote LLM (e.g. OpenAI credentials in
  `auth.json`). Local models need a `llama-server` binary, which is not included.

Extend with a derived image, for example:

```
FROM aigem-bot
USER root
RUN apk add --no-cache go    # for developer/tester bots that build Go projects
```

## Notes

- Runs as **root** by default so bind-mounted host dirs are writable out of the box. For
  non-root, pass `--user $(id -u):$(id -g)` and make sure the mounted dirs are owned to match.
- `auth.json` is a secret: transfer it over SSH, keep it `chmod 600`, and never commit it.
