# syntax=docker/dockerfile:1

# ---- build: static, CGO-free aigem binary ----
# Pinned to the build host's platform so a multi-arch buildx run cross-compiles
# with Go instead of emulating a foreign arch under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
# Module download is its own cached layer, so source edits do not re-fetch deps.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Stamped by the release workflow so `aigem version` in the image is meaningful;
# a plain `docker build` leaves the same "dev" placeholder a local build gets.
ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""
# TARGETOS/TARGETARCH come from buildx, so one Dockerfile cross-builds every arch.
ARG TARGETOS=linux
ARG TARGETARCH
# CGO off -> fully static (runs on any base); -trimpath/-s/-w shrink and de-noise it.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/aigem ./cmd/aigem

# ---- runtime: slim image for `aigem bot start` ----
FROM alpine:3.24
LABEL org.opencontainers.image.title="aigem" \
      org.opencontainers.image.description="Terminal AI coding agent in Go, with a Bubble Tea TUI and multi-agent chat bots." \
      org.opencontainers.image.source="https://github.com/gigovich/aigem" \
      org.opencontainers.image.licenses="Apache-2.0"
# bash: the bot's bash tool runs `bash -c` (developer/tester roles).
# git:  developer/tester shell out to git in their workdir.
# ca-certificates: HTTPS to the LLM API and Brave search.
# tini: PID 1 that reaps the bash subprocesses bots spawn and forwards signals.
RUN apk add --no-cache ca-certificates bash git tzdata tini

# Config and state are bind-mounted. aigem always namespaces under an "aigem" subdir
# (configDir = $XDG_CONFIG_HOME/aigem, StateDir = $XDG_STATE_HOME/aigem), so the data
# lands at /config/aigem and /state/aigem - bind-mount the host dirs onto those:
#   host ~/.config/aigem (or macOS ~/Library/Application Support/aigem) -> /config/aigem
#        bots/<name>/bot.yaml + memory/
#   host ~/.local/state/aigem                                          -> /state/aigem
#        auth.json (tokens), sessions/
ENV XDG_CONFIG_HOME=/config \
    XDG_STATE_HOME=/state

COPY --from=build /out/aigem /usr/local/bin/aigem
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

VOLUME ["/config/aigem", "/state/aigem"]
# A bot's working directory; bind-mount the repo a developer bot edits here.
WORKDIR /workspace

# tini -> entrypoint -> exec aigem, so SIGTERM reaches the bot for a graceful stop.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
