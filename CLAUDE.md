# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## What this repo is

`sal`, a CLI over [secure-agent-lab](https://github.com/lpezet/secure-agent-lab)
— the Docker stack that runs autonomous agents without exposing long-lived
credentials to the agent's process.

**This file records the decisions that were settled before any code existed**,
so they are not re-litigated or silently contradicted by the implementation.
Where something is genuinely open, it says so rather than guessing.

**The problem it exists to solve.** Today the unit of adoption for the stack is
"copy an example directory and edit it". The image is versioned by tag, but
`proxy/*.py`, `broker/*.js` and `cred-gateway/*.conf` are bind-mounted from the
deployment and do **not** move when the tag does. So a deployment can repin to a
release containing a security fix and keep running the vulnerable file, because
the fix landed in a file it owns a copy of. A tool that *installed* those files
is a tool that can *update* them. That is the whole point; features that do not
serve it are secondary.

## Why this is a separate repo from the stack

Not an org-chart decision — it protects a property the stack repo states
outright: *it versions the security boundary, not the code*. That is why most of
its changelog entries need no "Upgrading" section, and it is what lets someone
read `v1.5.0 → v1.9.0` and know that four things happened to their boundary.

Put the CLI in that repo and the property does not degrade, it disappears: CLI
work would drive the tag stream, and a deployment on `v1.5.0` seeing `v1.9.0`
could not tell whether any of it touched the boundary without reading four
changelogs. On a security tool, an alarm that is usually noise is worse than no
alarm.

The same reasoning rules out git submodules in either direction. A submodule
pointer is a commit in the parent repo, so submoduling this repo into the stack
would put CLI churn back into the tag stream in the least visible way possible.

## The design constraint that shapes everything

**The bank is data. `sal` is a generic installer over it. There is no
per-provider code in this repo, ever.**

`sal providers add stripe` must work by someone dropping `bank/stripe/` into the
stack repo, with zero changes here. If a provider ever needs bespoke CLI code,
the two repos are coupled and would have to ship together — which is exactly
what the split was meant to make structurally impossible.

The bank already holds up its end: `bank/schema/provider.schema.json` states the
manifest contract, and `bank/README.md` sets the bar for an entry as *"someone —
or something — holding only `bank/<name>/` and a running stack can install the
provider without reading anything else in this repo."* That "or something" is
this CLI.

**Suggested control, cheap and worth having early:** a test that fails if any
bank entry name (`github`, `anthropic`, `cloudflare`, `gcp`, …) appears as a
string literal anywhere in this repo's source outside fixtures. The moment that
test fails, someone has taught the CLI about a specific provider, and the
coupling is back.

## The contract between the two repos

Keep it narrow. A wide contract is how a split becomes a compatibility matrix.

1. **Stack and bank stay there**, tagged `vX.Y.Z` — still the version of the
   security boundary.
2. **`sal` lives here**, tagged on its own line, and is a client that fetches
   the bank **by stack tag**, not by checkout.
3. **`sal` declares a minimum stack version** and warns below it.
4. **The bank layout carries an explicit schema version.** This is the only
   thing that should ever break compatibility between the two repos, and it
   should change on the order of never. Implemented in stack `v1.9.0`:
   `schema_version` is required on every manifest, an integer, currently `1`.
   The rule is stated in the field's own description and is binding on this
   CLI — an installer supports a **fixed set** of generations and **refuses**
   anything higher, because a manifest from the future may declare a control
   the installer would silently skip. Hold a set, not an equality check against
   the schema's `const`, so that understanding two generations later is a data
   change.
5. **`sal --version` prints both** — its own version and the stack tag of the
   lab it is managing. Without this, "I am four versions behind" is ambiguous
   again and the split has bought nothing.
6. **`scripts/check-drift.sh` stays in the stack repo** even once `sal drift`
   exists. It is dependency-free bash that works for someone who never installs
   this CLI, and that is worth a little duplication.

## What the stack looks like, as the CLI sees it

Six services per deployment: `broker`, `proxy`, `cred-gateway`, `observer`,
`log-rotator`, `lab`. Two networks — `secure` (broker, proxy, cred-gateway) and
`lab` (lab, proxy, cred-gateway), the latter `internal: true` by default so the
proxy is the only way out. `observer` and `log-rotator` are on **neither**, on
purpose: they reach the shared `audit-logs` volume without becoming a channel
between the two sides. Never "fix" that by giving them a network.

A bank entry is up to four files plus a manifest:

```
bank/<name>/
  provider.json                  the manifest
  broker/<name>.js          →    /app/providers/<name>.js
  proxy/<name>.py           →    /addons/NNN_<name>.py     ← installer assigns NNN
  cred-gateway/<name>.conf  →    /etc/nginx/gateway.d/<name>.conf
  lab/setup.sh                   optional fragment, sourced by the deployment
```

Manifest fields that carry weight for an installer:

- **`schema_version`** — check it **first**, before anything else in the file is
  read as meaningful. Refuse a generation above the supported set; see contract
  item 4.
- **`min_stack`** — check it **second**, before writing any file. It is the one
  failure that is silent at install and fatal at runtime (`MODULE_NOT_FOUND` on
  `require("../audit")`). Note the format mismatch: `min_stack` is bare
  (`1.7.0`, per its `^[0-9]+\.[0-9]+\.[0-9]+$` pattern) while the thing it is
  compared against is a tag (`v1.9.0`). Normalize in one place, with a test.
- **`load_band`** — `policy` = 000–009, `provider` = 010–899, `post` = 900+. The
  addon file in the bank has **no** numeric prefix; the installer assigns the
  lowest free slot in the band. Never bake a number into a bank entry. Note the
  three enum values are machine-readable from the schema but **the numeric
  ranges are not** — they exist only in the `load_band` description prose. So
  slot assignment is the one constant this CLI must restate, and the one place
  it can silently desync from the stack. Say so in a comment where it is
  restated.
- **`broker_routes`** — every route the provider registers, each with `exposed`.
  `exposed: false` must appear in **no** `.conf` in **any** entry. Exposing
  `/github/token` or `/anthropic/cred` would hand the lab a reusable secret.
- **`secrets` vs `config`** — different storage, different permissions,
  different prompt. A secret is a file under the secrets dir; config is a value
  in `.env`. Never conflate them.
- **`hosts`** — must agree exactly with the quoted hostname literals in the
  addon, both directions.

**Three version checks, three severities. Do not collapse them into one
helper** — each is wrong at either of the others' severity:

| check | source | on failure |
|---|---|---|
| `schema_version` above the supported set | manifest | **refuse** — may be silently skipping a control |
| `min_stack` above the deployment's pinned tag | manifest | **refuse at install** — otherwise silent now, fatal at runtime |
| deployment's tag below `sal`'s declared minimum | this CLI | **warn** (contract item 3) |

**Record what was installed in `.sal/installed.json`** in the deployment — name,
assigned `NNN`, files written. `check-drift.sh` in the stack repo already looks
for it and degrades to filename-guessing without it. Record the **resolved
commit SHA** alongside the stack tag it was fetched from: a tag is mutable, so
the tag alone does not say what was actually installed.

## Non-obvious invariants

**`sal` is a host-side tool and never ships inside the lab image.** This is a
boundary property, not an ergonomic one. `sal observer open` cannot work from
inside the lab by construction — observer is on neither network and binds
loopback on the *host*. But the reason to state it as a rule is `sal secrets
set` and `sal providers add`: both are boundary-widening operations, so a `sal`
on `PATH` inside the lab would hand the agent a supported interface for widening
its own whitelist. Same family as the read-only `.devcontainer` shadow mount
that stops the agent editing `gateway.d/`, one level up. Do not add `sal` to a
lab image, a devcontainer feature, or a `lab_setup` fragment.

**One stack per project, never a shared stack.** Sharing would put two projects
behind one proxy, one audit trail and one set of injected credentials — an agent
working on project A holding credentials scoped for project B, and a trail that
cannot say which project provoked a line. Per-project also gets volume isolation
free, since Docker scopes volumes to the compose project: each lab trusts only
its own CA and each trail describes exactly one project.

The cost is real and should be surfaced, not hidden: six containers per project,
and **a forgotten lab is not idle** — it is a live credential-injecting proxy
with the secrets directory mounted. `sal labs list` is the answer to "what is
currently running with my credentials attached", which makes it a control rather
than a convenience.

**Never assign a host port. Ask Docker for it.** Publish observer as
`127.0.0.1::9000` — empty host port, Docker assigns — then read the assignment
back with `docker compose -p <project> port observer 9000`. Collisions become
structurally impossible rather than something tracked in a lockfile, and the
loopback-only binding survives untouched. Keep the `127.0.0.1` prefix: observer
publishes the audit trail over plain HTTP with no auth, and it is only safe
because it is not reachable off the host.

**Never take a credential value as an argv.** `sal secrets set` reads from the
TTY with echo off. An argv is in shell history, in `ps`, and in any process
listing the agent can run. Same rule for anything that shells out.

**Mount `secrets/`, never its parent.** The consolidated location is
`~/.config/secure-agent-lab/secrets/` (replacing the stack's current
`~/.config/agent-creds/` — needs a migration path). That parent will also hold
config, the install manifest and probably a bank cache; none of it belongs in
the broker. `0700` on the directory, `0600` on files.

**Record a credential's kind, never re-derive it later.** Anthropic has two
genuinely different credentials: an OAuth token (`sk-ant-oat01-`) tied to a
Claude subscription, sent as `Authorization: Bearer`, and an API key
(`sk-ant-api03-`) tied to a Console org, billed per token, sent as `x-api-key`.
Different auth systems, different revocation, different wire format.

`sal` needs almost no machinery for this, because the broker already encodes the
answer in the *filename*: it reads `ANTHROPIC_AUTH_TOKEN_PATH` first and falls
back to `ANTHROPIC_API_KEY_PATH`, deriving the type from which it found. So
`sal secrets set` only chooses a destination filename. Offer
`--type oauth|api-key`, default to detecting from the prefix, but **write the
decision down** — guessing a credential's kind from its shape later is the same
vendor-shape bet the stack's audit-leak suite exists to pin down.

## Command grammar

`gcloud`-shaped: `sal GROUP | COMMAND ...` — group(s), then verb, then
positional, with a small set of bare top-level commands, exactly as `gcloud`
keeps `init` and `info` alongside `gcloud storage`.

```
sal providers add cloudflare
sal providers create telegram --template rest-bearer
sal secrets set anthropic
sal features list
sal features enable observer
sal features disable observer
sal observer open
sal observer tail
sal labs list
sal open
```

**Lifecycle verbs are uniform, so they live in one group; a feature's own verbs
live in its group.** `gcloud` does both: `gcloud services enable
run.googleapis.com` for lifecycle, `gcloud run deploy` for the service's own
actions — and notably `gcloud services disable run.googleapis.com`, never
`gcloud run disable`. Enable, disable and list are the same operation for every
feature, so if each feature owns a copy there is no single place to answer "what
is on?". Turning observer off is `sal features disable observer`, not
`sal observer remove`.

**A feature earns a top-level group when it has verbs no other feature has.**
Observer earns one: `open` and `tail` are specific to a thing that serves a URL
and a stream. The egress allowlist would not — it is enable/disable plus a
config file, so it stays a `features` citizen. That rule is what keeps
group-first from degenerating into one top-level group per feature.

**Bare commands act on the lab in this directory; the plural noun manages the
set.** `sal open`, `sal up`, `sal down`, `sal init` are here-and-now
conveniences. `sal labs list` and `sal labs down --all` act across the machine,
and the plural is what stops them reading like they act on the current project.

`sal observer open` prints the URL **first**, then attempts a browser unless
`--no-open` — so the URL survives in scrollback when the launch silently fails,
which it does over SSH, in WSL, and inside a dev container. That last case is
the main path for this project, not an edge case. `sal observer tail` is the
answer for a terminal with no browser at all.

## Language and build

**Go, with `spf13/cobra`.** Settled 2026-08-11. The requirement was a single
binary the install script can drop on `PATH`; what decided it between the
candidates that clear that bar was distribution and one test.

**Distribution.** GoReleaser cross-compiles darwin/arm64, darwin/amd64,
linux/amd64 and linux/arm64 from one runner, emits `checksums.txt`, signs with
cosign and attaches SLSA provenance. That is the mitigation the Install section
below asks for, at near-zero cost. Rust reaches the same place with more
cross-toolchain friction and no benefit here — nothing in this tool is
perf-sensitive or memory-safety-critical, and the property reviewers will want
from a security tool is a small auditable dependency tree, not `unsafe`-freedom.
A Bun/Deno `--compile` binary embeds a full JS runtime and network stack, which
is poor optics for a tool whose pitch is a narrow boundary.

**The test.** The no-per-provider-code control suggested above is `go/parser` +
`go/ast` walking `*ast.BasicLit` across the packages — a structural check in
stdlib, not a grep that false-positives on comments and fixture paths. The
invariant this repo most needs to hold gets a compiler-grade guard for ~40
lines.

**Cobra** because its command tree *is* `sal GROUP VERB POSITIONAL` with bare
top-level commands alongside, which is the grammar below exactly, and shell
completion comes free. Dependency tree is `cobra` + `pflag` + `mousetrap`.

Consequences worth stating, since each had a plausible alternative:

- **Shell out to `docker compose`. Do not link `compose-go` or the Docker SDK.**
  The compose file in the stack repo is the source of truth and its semantics
  are enormous; the `docker` CLI is the stable contract. It also means `sal`
  never needs the Docker socket itself.
- **Fetch the bank over HTTPS, not git.** `net/http` + `archive/tar` +
  `compress/gzip` against `codeload.github.com/.../tar/refs/tags/<tag>` — no git
  dependency on the user's machine, which is what contract item 2 asks for.
- **Decode manifests into structs; do not pull a JSON-schema library.** The
  schema is `additionalProperties: false` with a closed field set, so
  `json.Decoder.DisallowUnknownFields()` *is* that check, with a better error
  than a validator's JSON-pointer syntax. Where a constant is machine-readable
  from the schema, read it from there rather than restating it — that is how the
  stack's own lint is built, and it is why bumping a value there bumps it here.
- **Reading a secret is `golang.org/x/term` with echo off, and it must hard-fail
  when stdin is not a TTY** rather than quietly accepting a pipe. A pipe is an
  argv one process away.

## Install

Bun's pattern — `curl -fsSL …/install | bash`, with an optional version
argument:

```
curl -fsSL https://github.com/lpezet/secure-agent-lab-cli/install | bash
curl -fsSL https://github.com/lpezet/secure-agent-lab-cli/install | bash -s "v1.2.0"
```

**The version there pins the binary, not the lab.** Those must stay on separate
lines: if the install command also pinned the stack, upgrading your CLI would
silently move everyone's security boundary, and pinning your boundary would mean
running a stale CLI forever. The lab a project uses is pinned per-project and
rewritten by `sal upgrade`.

`curl … | bash` as the install path for a security tool is a look, and reviewers
will say so. Cheap mitigations, all worth doing: publish a checksum, keep the
script short enough to actually read, and document clone-and-inspect as a
first-class alternative rather than a footnote.

## Still open

- **What `sal open` does when the current directory already has a
  `.devcontainer`.**
- **The migration path** from the stack's `~/.config/agent-creds/` to
  `~/.config/secure-agent-lab/secrets/`.

## What stays in the stack repo

Do not port these here, even when it would be convenient:

- `scripts/check-drift.sh` and `scripts/check-invariants.sh` — dependency-free
  bash, must work without this CLI installed.
- `PLAYBOOK.md`'s generation constraints — how to write a provider *from
  scratch*, which is precisely the case a bank of finished entries cannot cover.
- The bank itself, and its schema. Provider files are coupled to the *image*
  API (`require("../audit")` resolves only from 1.1.0; `import audit` depends on
  `PYTHONPATH=/opt/agent-proxy`), so they ship and version with the boundary
  they run against.

When something here needs a change over there, that is a PR to the stack repo
against its current `release/*` branch — never a workaround on this side.
