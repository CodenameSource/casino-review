#!/bin/sh
# Wrapper for launchd/cron: load .env (kept out of the plist so the token isn't
# stored in plaintext there), then exec the watcher. Resolves its own directory
# so it works regardless of where it's invoked from.
set -e
DIR="$(cd "$(dirname "$0")/.." && pwd)"

if [ -f "$DIR/.env" ]; then
	set -a
	. "$DIR/.env"
	set +a
fi

exec "$DIR/casino-review" "$@"
