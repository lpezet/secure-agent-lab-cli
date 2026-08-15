#!/usr/bin/env bash
# Runs the install-script tier inside containers, which is where the failures
# that matter actually live.
#
# run.sh beside this one tests the script's LOGIC — its refusals, its
# resolution, what it says — and it runs on whatever machine you have. This
# tests the assumptions underneath that logic, and they only vary by distro:
#
#   - Alpine's sha256sum and tar are busybox, which has no long options at all.
#     --ignore-missing and --status are GNU spellings, and install.sh cannot use
#     them.
#   - Alpine has no bash. `curl … | bash` on Alpine needs bash installed first,
#     and finding that out here is better than finding it out in an issue.
#   - As a non-root user, ~/.local/bin may not exist yet; the script has to
#     create it rather than assume it.
#
# This is exactly the tier CLAUDE.md says a container is right for — and note
# what it does NOT do: no Docker socket is mounted, nothing starts a lab. It
# runs a shell script against a filesystem. The lab lifecycle is the opposite
# case, and mounting the socket for it would silently break the property under
# test.
#
# Exit codes: 0 pass · 1 a container failed · 2 cannot run.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)

command -v docker >/dev/null 2>&1 || {
  printf 'docker is required for this tier; tests/install/run.sh covers the logic without it\n' >&2
  exit 2
}
docker info >/dev/null 2>&1 || {
  printf 'the docker daemon is not reachable\n' >&2
  exit 2
}

# `bash:5` is the official image and is ALPINE-based, which is the point: it
# brings busybox tar and busybox sha256sum — the implementations with no long
# options — without needing a package install, so every run here can keep
# --network none.
#
# What it does not cover, and should be said out loud rather than implied: a
# plain Alpine has no bash at all, so `curl … | bash` there needs `apk add bash`
# first. That is a fact about Alpine to document, not something this tier can
# fix.
images=(
  "debian:stable-slim"
  "ubuntu:24.04"
  "bash:5"
)

failed=0

for image in "${images[@]}"; do
  for user in root nonroot; do
    printf '\n--- %s as %s\n' "$image" "$user"

    # The repository is mounted READ-ONLY. Nothing in this tier writes into a
    # checkout, and mounting it writable would let a bug in a test do so.
    #
    # bash is invoked through `command -v` because it is not in the same place
    # everywhere: /bin/bash on Debian and Ubuntu, /usr/local/bin/bash on the
    # Alpine-based image.
    if [ "$user" = root ]; then
      cmd='exec "$(command -v bash)" /repo/tests/install/run.sh'
    else
      # A real unprivileged user rather than --user with a uid nothing knows
      # about: the script reads $HOME, and a uid with no home directory is a
      # different test from an ordinary user.
      cmd='(adduser -D tester 2>/dev/null || useradd -m tester) >/dev/null 2>&1
           su tester -c "$(command -v bash) /repo/tests/install/run.sh"'
    fi

    if docker run --rm \
      --network none \
      -v "$root":/repo:ro \
      "$image" sh -c "$cmd"; then
      printf '    %s/%s ok\n' "$image" "$user"
    else
      printf '    %s/%s FAILED\n' "$image" "$user"
      failed=$((failed + 1))
    fi
  done
done

printf '\n%d container run(s) failed\n' "$failed"
[ "$failed" -eq 0 ]
