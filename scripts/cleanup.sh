#!/bin/sh
# Manually prune committed GIFs older than ASSETS_TTL from the assets branch.
# Override the window inline, e.g. purge everything now:
#   ASSETS_TTL=0s ./scripts/cleanup.sh
# Loads .env the same way the service wrapper does, then runs one prune pass.
set -e
DIR="$(cd "$(dirname "$0")/.." && pwd)"

if [ -f "$DIR/.env" ]; then
	set -a
	. "$DIR/.env"
	set +a
fi

exec "$DIR/casino-review" cleanup
