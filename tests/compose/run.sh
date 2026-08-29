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
#   - `build --build-arg` with no `args:` in the file is what `sal up
#     --build-arg` is, and the environment reaching a build arg is what it is
#     not.
#
# The lab's own images are NOT built here: this is about compose's semantics,
# not about the stack. Two busybox services, plus a two-line Dockerfile for the
# build-arg section, are enough — and keep the tier fast enough to run on every
# change.
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
	docker compose -p "$project-build" -f "$work/build.yaml" down --rmi local --remove-orphans >/dev/null 2>&1
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
    # Reads a bind-mounted file ONCE at startup, which is the shape of every
    # boundary file in a real deployment: the proxy reads the allowlist and its
    # addons, the cred-gateway reads gateway.d, the broker reads its providers,
    # and the lab runs setup.d — all at container start, all through mounts.
    command: sh -c 'cat /conf; sleep 600'
    volumes:
      - ./conf:/conf:ro
  watcher:
    profiles: ["watcher"]
    image: busybox:latest
    command: sleep 600
    ports:
      - "127.0.0.1::9000"
YAML

# Written before anything runs: Docker creates a missing bind source as a
# root-owned DIRECTORY, which is the same reason sal creates lab/setup.d itself
# rather than letting the mount do it.
printf 'first\n' > "$work/conf"

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

# -------------------------------------------------- a changed mounted file
#
# The three behaviours `sal up` is built on, and the reason it restarts at all.
# A provider's files and the egress allowlist arrive by bind mount, so changing
# one changes the FILE and not the container's config — compose finds nothing
# to do and the running container keeps what it read at startup. Every message
# in sal that says "run `sal up`" is wrong unless sal closes that itself.
#
# Found the hard way: `sal allowlist allow` said to run `sal up`, `sal up`
# reported the lab up, and the proxy went on denying the destination.
out=$(compose logs worker 2>/dev/null)
check "a service reads its bind-mounted file at startup" \
	"$(printf '%s' "$out" | grep -q 'first' && echo 0 || echo 1)"

printf 'second\n' > "$work/conf"
compose up -d >/dev/null 2>&1
out=$(compose logs worker 2>/dev/null)
check "up does NOT re-read a changed bind-mounted file" \
	"$(printf '%s' "$out" | grep -q 'second' && echo 1 || echo 0)"

# How sal decides WHAT to restart. Only a service that was already running
# needs it — one compose has just created read the files on the way up.
#
# It names a service behind an ENABLED profile too, which is what sal needs: a
# feature that is on is as much a running container as any other, and asking a
# question that skipped it would leave it holding a stale file forever.
out=$(compose ps --services --status running 2>/dev/null | sort | tr '\n' ' ')
check "ps --services --status running names every running service" \
	"$([ "$out" = "watcher worker " ] && echo 0 || echo 1)"

# And the operation itself. Restart rather than --force-recreate on purpose:
# recreating throws away the container filesystem, so anything an agent
# installed inside the lab would vanish on an unrelated `sal up`.
compose restart worker >/dev/null 2>&1
out=$(compose logs worker 2>/dev/null)
check "restart DOES re-read it" \
	"$(printf '%s' "$out" | grep -q 'second' && echo 0 || echo 1)"

# Why `sal up` restarts everything EXCEPT the two services on no network.
# Docker assigns the host port when the file names none, and it picks a new one
# on restart — so restarting the observer for nothing moves the audit trail's
# URL, taking an open browser tab and any `sal observer tail` with it. The
# services that enforce the boundary have no published port, so they pay
# nothing for the same operation.
before=$(compose port watcher 9000 2>/dev/null)
compose restart watcher >/dev/null 2>&1
after=$(compose port watcher 9000 2>/dev/null)
check "restart REASSIGNS a host port the file left to Docker" \
	"$([ -n "$before" ] && [ -n "$after" ] && [ "$before" != "$after" ] && echo 0 || echo 1)"

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

# ---------------------------------------------------------------- build args
#
# Why `sal up --build-arg` is a separate `docker compose build` rather than a
# flag on the `up`. Three behaviours, and the third is the one that makes this
# implementable on sal's side at all.
#
# The declarative answer — `args:` on the lab service — is not open to us:
# compose.yaml is fetched verbatim from the stack repo, `sal drift` compares it
# against a fresh render so a local edit is a finding, and `sal upgrade`
# rewrites it so the edit does not survive. Adding it there would also mean the
# stack template naming variables that belong to an operator's own Dockerfile.
bproject="$project-build"
mkdir -p "$work/img"
cat > "$work/img/Dockerfile" <<'DOCKERFILE'
FROM busybox:latest
ARG SAL_PROBE=unset
RUN printf '%s\n' "$SAL_PROBE" > /probe
DOCKERFILE
cat > "$work/build.yaml" <<'YAML'
services:
  builder:
    build: ./img
    command: cat /probe
YAML

bcompose() { docker compose -p "$bproject" -f "$work/build.yaml" "$@"; }
# -T because this runs unattended: compose asks for a TTY by default and there
# is not one in CI.
probe() { bcompose run --rm -T builder 2>/dev/null | tr -d '\r\n'; }

if ! bcompose build >/dev/null 2>&1; then
	printf '  cannot build the probe image; is the daemon healthy?\n' >&2
	exit 2
fi

# The failure that produced the issue: exporting a variable and rebuilding does
# nothing. Compose reads the environment for ${...} interpolation in the file
# and for valueless `environment:` entries, which are RUNTIME. Neither is a
# build arg, so `CLAUDE_VERSION=$(curl ...) sal up --build` built with the
# Dockerfile's default and said nothing.
SAL_PROBE=1.2.3 bcompose build >/dev/null 2>&1
check "the ambient environment does NOT reach a build arg" \
	"$([ "$(probe)" = "unset" ] && echo 0 || echo 1)"

# And what sal does instead. Note there is no `args:` in build.yaml above —
# that is the whole point, because sal cannot put one there.
bcompose build --build-arg SAL_PROBE=1.2.3 >/dev/null 2>&1
check "--build-arg KEY=VALUE works with NO args: declared in the file" \
	"$([ "$(probe)" = "1.2.3" ] && echo 0 || echo 1)"

# The bare form, which is what makes `CLAUDE_VERSION=$(curl ...) sal up
# --build-arg CLAUDE_VERSION` read the way an operator expects. sal passes the
# flag through unaltered, so this is compose's behaviour and not sal's.
SAL_PROBE=9.9.9 bcompose build --build-arg SAL_PROBE >/dev/null 2>&1
check "a bare --build-arg KEY takes the value from the environment" \
	"$([ "$(probe)" = "9.9.9" ] && echo 0 || echo 1)"

printf '\n%d passed, %d failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
