#!/usr/bin/env bash
# Pins the `docker compose` behaviours sal depends on, against the real binary.
#
# sal shells out to the docker CLI rather than linking a library, on the
# grounds that the CLI is the stable contract. This is what makes that a
# checked claim rather than a hope. Everything here is something sal's code
# assumes, and every one of them was written from documentation and reasoning
# before it could be run:
#
#   - `config --profiles` is how `sal features list` learns what a lab has.
#   - COMPOSE_PROFILES in .env is how a feature stays on across `sal up` and a
#     hand-run `docker compose up` alike.
#   - `port` is how `sal observer open` gets a URL, and its failure shape when
#     nothing is running is what that command turns into a sentence.
#   - `ps --quiet SERVICE` is how `sal open` decides the lab is up.
#
# The lab's own images are NOT built here: this is about compose's semantics,
# not about the stack. A tiny image and two services are enough, and keep the
# tier fast enough to run on every change.
#
# Exit codes: 0 pass · 1 a behaviour is not what sal assumes · 2 cannot run.
set -uo pipefail

command -v docker >/dev/null 2>&1 || { printf 'docker is required\n' >&2; exit 2; }
docker compose version >/dev/null 2>&1 || { printf 'the docker compose plugin is required\n' >&2; exit 2; }
docker info >/dev/null 2>&1 || { printf 'the docker daemon is not reachable\n' >&2; exit 2; }

# Named with the PID so a run never collides with a real lab or another run,
# and torn down by the trap whatever happens.
project="sal-compose-test-$$"
work=$(mktemp -d "${TMPDIR:-/tmp}/${project}-XXXXXX") || exit 2
cleanup() {
	docker compose -p "$project" --profile watcher -f "$work/compose.yaml" down --volumes --remove-orphans >/dev/null 2>&1
	rm -rf "$work"
}
trap cleanup EXIT

passed=0 failed=0
ok()  { passed=$((passed + 1)); printf '  ok      %s\n' "$1"; }
bad() { failed=$((failed + 1)); printf '  FAILED  %s\n' "$1"; }
check() { if [ "$2" -eq 0 ]; then ok "$1"; else bad "$1"; fi; }

# Two services shaped like the ones that matter: one always-on, and one behind
# a profile publishing a loopback port with no host port chosen — which is how
# the observer is published and why a collision between labs is impossible.
cat > "$work/compose.yaml" <<'YAML'
services:
  worker:
    image: busybox:latest
    command: sleep 600
  watcher:
    profiles: ["watcher"]
    image: busybox:latest
    command: sleep 600
    ports:
      - "127.0.0.1::9000"
YAML

compose() { docker compose -p "$project" -f "$work/compose.yaml" "$@"; }

printf '\ndocker compose semantics sal depends on\n'
printf '  (compose %s)\n' "$(docker compose version --short 2>/dev/null)"

# ------------------------------------------------------------------ profiles
#
# `sal features list` reads this. A profile has to be reported whether or not it
# is currently enabled, or a disabled feature would vanish from the listing
# instead of showing as off.
printf '# nothing enabled\n' > "$work/.env"
profiles=$(compose config --profiles 2>/dev/null)
check "config --profiles reports a profile that is NOT enabled" \
	"$([ "$profiles" = "watcher" ] && echo 0 || echo 1)"

services=$(compose config --services 2>/dev/null | sort | tr '\n' ' ')
check "a profiled service is excluded when COMPOSE_PROFILES is absent" \
	"$([ "$services" = "worker " ] && echo 0 || echo 1)"

# THE reason `sal init` writes COMPOSE_PROFILES into .env rather than relying on
# a default: compose's own reading of an absent value is "nothing enabled", so
# a lab whose .env never mentioned it would come up with no observer at all.
printf 'COMPOSE_PROFILES=watcher\n' > "$work/.env"
services=$(compose config --services 2>/dev/null | sort | tr '\n' ' ')
check "COMPOSE_PROFILES in .env enables the profile" \
	"$([ "$services" = "watcher worker " ] && echo 0 || echo 1)"

# And the reason sal ALSO passes --profile on every call: it must not depend on
# compose reading a file the way sal believes it does.
printf '# nothing enabled\n' > "$work/.env"
services=$(docker compose -p "$project" --profile watcher -f "$work/compose.yaml" config --services 2>/dev/null | sort | tr '\n' ' ')
check "--profile enables it without .env saying so" \
	"$([ "$services" = "watcher worker " ] && echo 0 || echo 1)"

# ---------------------------------------------------------- a stopped project
#
# `sal observer open` turns this into "the lab is not running", and `sal open`
# into "there is nothing to open a shell in". Both read an ERROR or an empty
# answer as the same finding, which is what these two pin.
out=$(compose port watcher 9000 2>/dev/null)
status=$?
check "port on a service with no container fails or answers nothing" \
	"$([ "$status" -ne 0 ] || [ -z "$out" ] && echo 0 || echo 1)"

out=$(compose ps --quiet worker 2>/dev/null)
check "ps --quiet answers nothing for a service with no container" \
	"$([ -z "$out" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------- running
printf 'COMPOSE_PROFILES=watcher\n' > "$work/.env"
if ! compose up -d >/dev/null 2>&1; then
	printf '  cannot start the probe project; is the daemon healthy?\n' >&2
	exit 2
fi

out=$(compose ps --quiet worker 2>/dev/null)
check "ps --quiet answers an id for a running service" \
	"$([ -n "$out" ] && echo 0 || echo 1)"

# The whole reason sal never picks a host port: it asks Docker which one it
# assigned, so two labs cannot collide. The loopback prefix has to survive —
# the audit trail is served over plain HTTP with no auth, and is only safe for
# not being reachable off the host.
url=$(compose port watcher 9000 2>/dev/null)
check "port answers host:port for a running service" \
	"$(printf '%s' "$url" | grep -qE '^127\.0\.0\.1:[0-9]+$' && echo 0 || echo 1)"

# ------------------------------------------------------------------ removal
#
# What `sal features disable` does. sal passes --profile explicitly, and this
# pins what happens WITHOUT it — because that is the half a future compose
# could change under us, and the answer decides whether passing it is
# belt-and-braces or load-bearing.
printf 'COMPOSE_PROFILES=\n' > "$work/.env"
compose rm --stop --force watcher >/dev/null 2>&1
out=$(docker compose -p "$project" --profile watcher -f "$work/compose.yaml" ps --quiet watcher 2>/dev/null)
check "naming a service removes it even when its profile is off" \
	"$([ -z "$out" ] && echo 0 || echo 1)"

# Stopping one feature leaves the rest of the lab alone, which is what makes
# `sal features disable` something other than a small `sal down`.
out=$(compose ps --quiet worker 2>/dev/null)
check "removing one service leaves the others running" \
	"$([ -n "$out" ] && echo 0 || echo 1)"

printf '\n%d passed, %d failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
