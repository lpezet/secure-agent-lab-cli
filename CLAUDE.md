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
assigned `NNN`, files written — and the **resolved commit SHA** alongside the
stack tag: a tag is mutable, so the tag alone does not say what was actually
installed. Where the deployment lives, and why the record stays inside it, is
under Non-obvious invariants below.

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

**The deployment lives outside the project.** Two trees:

```
<project>/.sal/lab.json                     committable pointer: name + stack tag
~/.config/secure-agent-lab/labs/<name>/     the deployment itself, 0700
```

This is a boundary property, not tidiness. The agent works in the project, so a
deployment kept there is one the agent can edit — and proxy addons, broker
providers and gateway configs are exactly what it would want to edit. The
dev-container example needs a read-only shadow mount over its own
`.devcontainer` for precisely this reason. Out of the workspace they are not
merely unwritable but invisible, and the shadow mount disappears from the
generated compose because there is nothing left to shadow.

`<name>` is `<basename>-<8 hex of the project's absolute path>`. The suffix is
load-bearing: two projects called `api` must not resolve to one lab, because
sharing would put both behind a single proxy, a single audit trail and a single
set of injected credentials.

**The install record stays in the deployment**, at
`<lab>/.sal/installed.json` — `scripts/check-drift.sh` reads
`"$DEPLOY/.sal/installed.json"` and degrades silently to filename-guessing
without it. The nesting looks redundant inside a directory `sal` owns, and it
stays because it makes the lab an *ordinary* deployment that a tool which has
never heard of `sal` still works on. The project's `.sal/` holds `lab.json` and
must never hold an `installed.json`: two answers to "what is installed" and
nothing keeping them equal.

**Per-provider values never appear in the generated compose.** Every manifest
declares its own `secrets`, `config` and `lab_env`, so those become entries in
`.env` (broker and proxy) and `lab.env` (the lab only, so it never receives the
broker's environment), written by `providers add`. This is not only tidiness:
the AST invariant test reads `.go` files, so a provider name in a template would
sail straight past it. `internal/compose`'s `TestNoProviderNamesInOutput` checks
the rendered output for exactly that.

⚠️ **The compose template in `internal/compose/templates/` is a reference
implementation living here temporarily.** It describes the stack's wiring, which
is the boundary's business — so a change to the service graph currently needs a
`sal` release, which is the coupling the split exists to prevent. The intended
end state is the stack repo carrying it and `sal` fetching it at the pinned tag,
the way it already fetches the bank. Keep it in one file and keep it dumb, so
moving it is a copy. Note also that `stack/` in the stack repo is **not** that
template: `stack/broker/` is the broker's source, while a deployment's `broker/`
is the providers it loads. Same name, opposite roles.

**An upgrade rewrites files, and DELETES the ones the new release dropped.**
Repinning without rewriting is not an upgrade — that is the problem this repo
exists for. The deletion half carries its own risk and is easy to forget: a
cred-gateway config left behind keeps whitelisting a route the entry no longer
exposes, which is a widened boundary nothing would report. An entry keeps the
slot it was assigned, because the slot is the addon's filename prefix and
therefore its load order, and load order is a security property. And one
provider that cannot make the move refuses the WHOLE upgrade: half a deployment
on each of two releases is a boundary nobody can describe.

**A function that reads state must not quietly write it.** Slot observation
once appended what it found on disk to the install record, and its caller saved
that record — writing the stack's own proxy addons into `installed.json` as if
they were bank entries named `policy` and `allowlist`. `check-drift.sh` reads
that field to decide which entries a deployment claims, so a healthy lab
reported drift forever, and `upgrade` refused because no bank has those
entries. It reads into a set now. The record says which BANK ENTRIES are
installed; a stack addon is not one, and `base_addons` is where those live.

**There is no cache, and that was a deliberate reversal.** An early version
cached extracted trees under the config directory, keyed by commit, with a
tag→commit index and an `--offline` flag. It bought 0.54 seconds on a 209KB
download, for commands that run occasionally — in exchange for commit-keyed
directories, staleness rules, subtree slugs and an unbounded directory that
grew forever, all of which are state that can be wrong.

The reason it was never needed: **a deployment already records the commit it is
pinned to**, so every command against an existing lab knows exactly which tree
it wants and fetches it straight into a temporary directory. Only the commands
that CHOOSE a version — `init` and `upgrade --to` — resolve a tag at all, and
they do it once, explicitly. Fetch by commit, never by tag: it is what stops a
moved tag changing what an existing lab reads.

`--stack-dir` points at a local checkout instead of downloading. It replaced
`--offline` and is better in every direction: it names where content came from
rather than depending on what a hidden cache happened to hold, it works on a
machine that has never had network access, and it lets someone test an
unreleased branch. It is also what the test suite uses, so no test reaches the
network.

If repeated fetches ever become a real cost — a `drift` check on every CI run,
say — a cache can go back behind the same interface, justified by a measurement
rather than by an assumption.

**Extraction refuses every entry type except regular files and directories.**
This is the one place in `sal` where bytes off the network become files on disk.
A symlink in a source archive is a link that can be aimed outside the directory
it lands in, and a hardlink is worse; deciding which are safe is a harder
problem than having none. The bank contains neither, so nothing legitimate is
lost and a future entry that needs one gets a loud refusal rather than a quiet
extraction. Modes come from `sal`, not from the archive.

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

## Versioning this CLI

`sal` is tagged on its own line, separate from the stack. That is the whole
point of the split, and `sal --version` printing both is what keeps it legible.

**What SemVer covers here is wider than the flags.** The public surface is the
command grammar *and* three on-disk formats:

| Surface | Who else reads it |
|---|---|
| command grammar and flags | scripts, CI, people |
| `<project>/.sal/lab.json` | committed into a user's git history; read by whatever `sal` they have next month |
| `<lab>/.sal/installed.json` | **`scripts/check-drift.sh` in the stack repo**, by someone who may never have installed `sal` |
| the generated `compose.yaml` | regenerated by `sal upgrade`, so the least contractual of the three |

The two JSON files carry a required `schema_version`, and the rule is the
stack's own: support a fixed set of generations and **refuse** anything above
it, because a file from the future may carry a field this build would silently
ignore. Absence is refused too — guessing once which generation an unversioned
file is would make that guess permanent. See `internal/schema`.

**Pre-1.0 for now, and honestly so.** In the space of one week `--offline` and
`--refresh` were removed in favour of `--stack-dir`, a bank cache was
introduced and then deleted entirely, `installed.json` gained `base_addons`,
and lab discovery moved from project-local to two trees. Each would have been a
major bump. The design is still settling, and 0.x says that out loud.

**1.0.0 means all of these, not a feeling:**

1. Every command in the grammar works, or is removed from the grammar. A
   `--help` listing commands that exit 1 is not a 1.0.
2. Both JSON formats declared stable at their current generation.
3. The `lab_setup` question resolved — fragments install today and nothing
   sources them, so `github` and `gcp` install without fully working.
4. The install script exercised against a real published release.
5. A written compatibility statement: this table, and what a major bump means.

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

## Testing

Mirror the stack repo's conventions rather than inventing new ones — a facade
`run.sh` per tier, resources named with the PID and torn down by an `EXIT` trap
so a run never collides with a real stack, **exit 2 for a missing prerequisite**
so it is distinguishable from exit 1 for a failed assertion, and a tier that
needs credentials skipping rather than failing when it has none. Do not copy
their `lib.sh` across; the same reasoning that keeps `check-drift.sh` over there
applies to duplicating assertions here.

**Most of the assurance needs no Docker.** Assigning the lowest free `NNN` in a
band, writing `0600`/`0700`, never emitting an `exposed: false` route into a
generated `.conf`, recording `installed.json` accurately, detecting drift — all
of it is a temp directory and a fake bank. `tests/fixtures/` holds that bank,
under invented provider names so a fixture cannot quietly become the
per-provider code the invariants test exists to catch.

**The CLI surface is testscript** (`cmd/sal/testdata/script/*.txtar`), which is
where the grammar decisions above are actually observable — a unit test cannot
tell you that `sal observer disable` is not a command. Its `Setup` points `HOME`
at the script's scratch directory, which is a safety property rather than
hygiene: a test run that reached the operator's real secrets directory could
overwrite a live credential.

**A container is right for the install script, and wrong for the lab.** Testing
`curl … | bash` across `debian:slim`, `alpine` and `ubuntu`, as root and
non-root, catches what actually breaks — arch detection, busybox `tar`,
`sha256sum` vs `shasum`, which PATH directory is writable — and needs no Docker
inside the container.

⚠️ **Do not test the lab lifecycle by mounting `/var/run/docker.sock` into a
container.** It silently breaks the property under test. With the socket
mounted, compose drives the *host* daemon, so observer publishes on the host's
loopback while the test container has its own network namespace — `sal observer
open` prints a URL the test cannot reach. The failure looks like a bug in `sal`,
and the obvious "fix" is binding `0.0.0.0`, which destroys the only thing that
makes an unauthenticated audit trail safe. It is also root-equivalent on the
host, in the test harness of a security tool. Run the lifecycle tier against the
host daemon in a temp directory with a unique compose project name instead —
which is also how `sal` is actually deployed.

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
