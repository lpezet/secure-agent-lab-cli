#!/usr/bin/env bash
# Fixture entrypoint. Runs each installed provider's setup fragment, then hands
# off to the container's command — the shape stack 1.14.0 ships.
set -euo pipefail
SETUP_DIR="${AGENT_SETUP_DIR:-/etc/agent-setup.d}"
if [ -d "$SETUP_DIR" ]; then
  for f in "$SETUP_DIR"/*.sh; do
    [ -e "$f" ] || continue
    bash "$f"
  done
fi
exec "$@"
