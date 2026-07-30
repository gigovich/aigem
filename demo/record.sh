#!/usr/bin/env bash
# Regenerate docs/assets/demo.gif.
#
# aigem, its tools, and the markdown renderer in the recording are real; only the
# model is the scripted stub in demo/mockmodel. That makes the session repeatable
# - same prompt, same tool calls, same answer - and costs no credentials. The GIF
# itself is not byte-reproducible: it is recorded in real time, so frame timing
# varies between runs.
#
# Needs: vhs, ttyd, ffmpeg, go. See demo/README.md.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
port=${AIGEM_DEMO_PORT:-9411}

cleanup() {
  [[ -n ${mock_pid:-} ]] && kill "$mock_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

for cmd in vhs ttyd ffmpeg go; do
  command -v "$cmd" >/dev/null || { echo "missing dependency: $cmd" >&2; exit 1; }
done

echo "==> building aigem and the scripted model"
go build -o "$work/bin/aigem" "$repo/cmd/aigem"
go build -o "$work/bin/mockmodel" "$repo/demo/mockmodel"

# The sample project is copied out of the repo on purpose: run it in place and
# the project root walks up to this repository, so aigem would report on the
# repo's own skills and settings instead of the sample.
echo "==> preparing an isolated workspace"
mkdir -p "$work/notes" "$work/config/aigem" "$work/state/aigem"
cp "$repo"/demo/workspace/* "$work/notes/"

# An explicit "no search backend" config. Without the file present, the first
# interactive launch runs the web-search setup wizard, which would eat the
# recording's first keystrokes before the TUI ever opens.
echo '{"provider":""}' > "$work/state/aigem/search.json"

cat > "$work/config/aigem/models.json" <<EOF
{
  "providers": [
    {
      "id": "demo",
      "base_url": "http://127.0.0.1:$port",
      "api": "openai-completions",
      "auth": "none",
      "models": [
        {"provider": "demo", "id": "aigem-demo", "name": "aigem demo", "context_window": 262144}
      ]
    }
  ]
}
EOF

echo "==> starting the scripted model on port $port"
"$work/bin/mockmodel" -addr "127.0.0.1:$port" >"$work/mock.log" 2>&1 &
mock_pid=$!
for _ in $(seq 40); do
  curl -sf -m 1 "http://127.0.0.1:$port/health" >/dev/null && break
  sleep 0.25
done
curl -sf -m 1 "http://127.0.0.1:$port/health" >/dev/null || { echo "model stub did not start" >&2; cat "$work/mock.log" >&2; exit 1; }

echo "==> recording"
mkdir -p "$repo/docs/assets"
cd "$repo"
AIGEM_DEMO_DIR="$work/notes" \
XDG_CONFIG_HOME="$work/config" \
XDG_STATE_HOME="$work/state" \
PATH="$work/bin:$PATH" \
  vhs demo/demo.tape

echo "==> wrote docs/assets/demo.gif ($(du -h "$repo/docs/assets/demo.gif" | cut -f1))"
