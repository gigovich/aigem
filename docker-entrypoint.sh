#!/bin/sh
set -e

# Pass an explicit command straight through, e.g. `docker run IMAGE bot list`.
if [ "$#" -gt 0 ]; then
	exec aigem "$@"
fi

# Otherwise start the bot named by BOT_NAME (the common one-bot-per-container case).
if [ -n "${BOT_NAME:-}" ]; then
	exec aigem bot start "$BOT_NAME"
fi

# Neither a command nor BOT_NAME: the container is misconfigured. Exit non-zero
# so a restart policy or orchestrator sees a failure instead of a clean stop.
echo "aigem: set BOT_NAME=<bot> or pass a command (e.g. 'bot list')." >&2
exit 1
