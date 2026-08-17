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

**The base proxy addons are the deployment's business only below stack
1.10.0.** Under that release the image carries nothing and a deployment that
does not vendor `000_policy.py` has no internal-host block at all — the proxy
sits on both networks, so with no policy addon loaded it forwards to the broker
and the cred-gateway whitelist can be walked around entirely. `sal init`
therefore fetches and vendors them, and refuses outright if it cannot.

From 1.10.0 the proxy image carries both at `/opt/agent-proxy/addons/` and
loads them ahead of the `/addons` mount, so a vendored copy is not a smaller
control but none: the image's wins and the deployment's is skipped with a
warning naming the file. So `init` stops vendoring, `upgrade` DELETES what a
lab already vendored on the way past, and `drift` inverts — below the line an
unvendored addon is MISSING, at or above it a vendored one is a note.

`version.StackBakesAddons` is the one place that line is drawn, and a tag it
cannot parse takes the vendoring branch. That is the direction which fails
closed, and it is the same choice `scripts/check-drift.sh` makes for a pin that
is not a release tag.

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

`sal labs down <name>` is the other half of it, and the half that has to exist:
the labs most worth stopping are the ones `list` just reported as having no
project left, and `sal down` cannot reach those — it finds a lab *from* a
project, which is exactly what is missing. Names are spelled exactly, for the
same reason a credential is: two projects called `api` are what the hash suffix
exists to keep apart, so a prefix would be ambiguous precisely where it matters.

**`labs down` has no `--volumes`, though `sal down` does.** That flag deletes a
lab's audit trail — the record of everything the agent did through it — and
across a whole machine that is not an operation with a safe shape. Deleting one
trail is a decision; deleting every trail is one decision applied to things the
operator was not thinking about. `sal down --volumes` per project makes them
visit each. Stopping, by contrast, is reversible, so `labs down` does not prompt
at all — a confirmation on a reversible action only teaches people to clear
prompts.

**One lab that will not stop does not abandon the rest**, which is the opposite
of `upgrade`'s all-or-nothing rule and not an inconsistency. A partial upgrade
leaves half a deployment on each of two releases, a boundary nobody can
describe. A partial `down` just leaves fewer labs running — and aborting would
leave *more* of them up, which is the opposite of what was asked for. The exit
status is still non-zero, and the "of which were running" count only counts labs
that actually stopped: a lab that refused is still live, and counting it would
report the reverse of what happened.

**Never assign a host port. Ask Docker for it.** Publish observer as
`127.0.0.1::9000` — empty host port, Docker assigns — then read the assignment
back with `docker compose -p <project> port observer 9000`. Collisions become
structurally impossible rather than something tracked in a lockfile, and the
loopback-only binding survives untouched. Keep the `127.0.0.1` prefix: observer
publishes the audit trail over plain HTTP with no auth, and it is only safe
because it is not reachable off the host.

**`sal open` opens the LAB, and a dev container running for the same project
is a warning rather than a redirect.** The two look identical from a terminal
and are not the same thing: a dev container the operator brought themselves is
not on the `lab` network, so it does not go out through the proxy and nothing
it does reaches the audit trail. Opening it because it happened to be there
would hand someone a shell they believe is inside the boundary when it is not.

Telling them apart is a label, not a guess. The Dev Containers extension
stamps `devcontainer.local_folder=<project>` on what it starts, so
`docker ps` narrowed by that label answers "is there one for this project",
and the same query narrowed again by `com.docker.compose.project=<lab>`
answers "is it ours". What is in the first answer and not the second is
foreign, and gets the warning. A dev container that IS the lab's own service
needs no word — it is exactly where the command is about to put you.

**A lab that is not running has no observer URL, and that is the whole
answer.** Because the port comes from Docker rather than from anything `sal`
stores, there is nowhere to look it up — a stopped lab has no published port at
all. So `observer open` and `observer tail` say the lab is not running and name
`sal up`, rather than printing a URL that would fail to connect. Whether
`docker compose port` answers a stopped service with an empty line or an error
has varied between compose versions, so both are read as the same finding; the
daemon itself was already checked one step earlier.

**A browser that will not start is not a failed command.** `observer open`'s
job is to give you the URL, and it has done that before anything is launched.
Exiting non-zero when no launcher exists would report a command that did its
job as having failed — and the failure that matters is invisible anyway: a
launcher that exits 0 and displays nothing is the ordinary case in a dev
container. `$BROWSER` is honoured first on every platform, and `wslview` is
tried before `xdg-open` because on WSL `xdg-open` frequently exists and does
nothing. `sal` never waits for what it launched: `$BROWSER` is often a browser
rather than a launcher, and one with no window open yet runs in the foreground
until it is closed.

**The audit trail is rendered by shape, never by provider.** `observer tail`
reads the observer's `/events` stream and prints the three fields every writer
in the stack agrees on — `ts`, `service`, `event` — then whatever else the line
carried, as `key=value`. This is the no-per-provider-code rule reaching the one
place it would be easy to forget it applies: a formatter that gave `github` or
`anthropic` events their own layout would be vendor knowledge, and it would
silently hide fields it did not recognise. Nothing is dropped, including a line
that is not JSON at all.

**The stream has no end, so `--follow=false` ends on quiet.** Every connection
replays a backlog of recent events and then stays open, with no marker between
the two — so "print the history and exit" cannot be implemented against a
boundary, because there is none. It is defined instead as everything that
arrived before the stream went quiet. Conversely a stream that *ends* while
following means the observer's container went away, and that is an error: a
tail that exits 0 tells a script it finished normally when what it was watching
disappeared.

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

**The link is recorded in both directions, and only one of them is trusted.**
The project→lab direction is the pointer; the lab→project direction is
`project_dir` in the install record. It has to be *recorded* because the name
derives from the project's path through a hash, and a hash cannot be inverted —
without it an inventory of the labs directory cannot say what any lab is *for*.

The asymmetry is the point. The pointer is what `sal` acts on; `project_dir` is
a claim made when the lab was created, and `sal labs list` checks it rather
than reading from it. A project that was deleted, or that no longer points
back, is exactly the forgotten lab that command exists to surface — and a
deployment left running is not idle. So the check must not walk up the way
`lab.Find` does: an ancestor project's pointer would answer for a directory
that has none, and report a lab nothing points at as healthy. `lab.PointerAt`
is the non-walking form, and it exists for that one difference.

Note that an upgrade rebuilds the record from scratch, so a field it does not
carry forward is dropped silently. `project_dir` is written there from the
project `sal` actually resolved, which also fills it in for a lab created
before it existed.

**Adding an optional field to `installed.json` is NOT a generation event**,
which is the opposite of the rule for bank manifests — and the difference is
one line of code rather than a judgement call. A manifest is decoded with
`DisallowUnknownFields`, so a new field makes every older `sal` fail to read it
at all. The install record is decoded plainly, so an older `sal` ignores what it
does not know and still answers correctly every question it asks. A field that
changes the meaning of an existing one, or that a reader must honour to stay
correct, is a generation event wherever it lives.

**Per-provider values never appear in the generated compose.** Every manifest
declares its own `secrets`, `config` and `lab_env`, so those become entries in
`.env` (broker and proxy) and `lab.env` (the lab only, so it never receives the
broker's environment), written by `providers add`. This is not only tidiness:
the AST invariant test reads `.go` files, so a provider name in a template would
sail straight past it. `internal/compose`'s `TestNoProviderNamesInOutput` checks
the rendered output for exactly that.

**A feature is a compose PROFILE, and its service has the same name.** That
equivalence is the whole implementation of `sal features`, and it is what let
the feature question be answered without breaking the rule below: the template
gained one static key (`profiles: ["observer"]`), not a conditional, so it
still renders the same for every deployment and can still MOVE to the stack
repo as a copy. Which features are on is a VALUE — `COMPOSE_PROFILES` in the
deployment's `.env`, the variable compose reads itself, so a `docker compose
up` run by hand starts what `sal up` starts. `compose.TestEveryProfileIsAService`
holds the naming rule, without which a feature could list, enable and disable
while turning nothing on or off.

**An absent `COMPOSE_PROFILES` means every feature ON.** A lab created before
features existed, or one whose `.env` lost a line, must not come up quietly
serving no audit trail: a feature that fails on is visible, and one that fails
off is not. `init` writes the variable anyway and `upgrade` back-fills it,
because compose's own reading of an absent value is the opposite one — and a
divergence between what sal starts and what a hand-run compose starts is not
worth leaving in place.

**Disabling stops the service BEFORE it records the change; enabling records
before it starts.** Both orders point the same way: the state that must never
be reachable is a record saying a feature is on while nothing is running it,
because that is how someone comes to trust an audit trail that does not exist.
The opposite disagreement is untidy and visible — `sal features list` reports
what `.env` says and what Docker says, side by side, and warns when they differ
in a lab that is up. `features disable` never touches a volume: the trail the
observer was serving outlives the observer.

**The deployment's wiring is FETCHED, not rendered.** `sal` carried a copy of
the service graph in `internal/compose/templates/` until stack 1.12.0, with a
warning saying it was temporary — a change to the stack's wiring needed a `sal`
release, which is the coupling the two-repo split exists to prevent. That is
now gone: `template/deployment/` is fetched at the pinned commit and written
**verbatim**, because everything per-deployment in it arrives through `.env`
and `lab.env`.

Three things had to change over there first, and each is worth knowing because
each is a property `sal` depends on:

- the observer's host port had to be assignable rather than fixed, or two labs
  on one machine collide (stack #79);
- the observer had to sit behind a compose profile, or `features` has nothing
  to switch (stack #80);
- the file had to name no provider, or every lab's compose would list
  credential paths for entries it does not have — and `environment:` wins over
  `env_file:`, so those would override what a manifest declares (stack #95,
  #96).

**`version.TemplateFrom` is a floor, not a warning.** Below stack 1.12.0 the
template either does not exist at that path or names specific bank entries, so
`init` and `upgrade` refuse rather than half-support it, and `drift` says the
compose file was not compared instead of inventing a comparison. A lab created
before that release keeps working; `sal upgrade` is what moves it.

**Two values in `.env` are the wiring's, not a provider's**, and they are the
ones whose defaults are quietly wrong for a `sal`-managed lab:
`WORKSPACE_DIR` (whose default mounts a directory inside the deployment rather
than the project) and `AGENT_CREDS_DIR` (whose default is the stack's legacy
credential location, which `config.LegacySecretsDir` names and `sal`
deliberately does not use). `OBSERVER_PORT` is set EMPTY on purpose — the
template publishes `127.0.0.1:${OBSERVER_PORT-9000}`, and empty is what leaves
the port for Docker to assign.

**`sal up` RESTARTS what was already running, because `docker compose up` does
not.** Every boundary file in a deployment — a broker provider, a proxy addon,
a cred-gateway config, a `lab_setup` fragment, the egress allowlist — arrives
through a bind mount and is read at container start. So changing one changes
the *file* and not the container's config, and compose correctly finds nothing
to do: the running container keeps what it read when it started.

Five messages already told the operator to run `sal up` after changing one, and
all five were right about what should happen. The command was what was wrong.
It surfaced as `sal allowlist allow` reporting a destination permitted, `sal up`
reporting the lab up, and the proxy going on denying it — and it is worse after
`providers remove`, where the cred-gateway keeps whitelisting a route that is
gone. Fixing the wording instead would have left the operator responsible for
knowing that mounted config is not re-read, which is precisely the knowledge
this tool exists to hold. A `--restart` flag is the same mistake with a longer
name: the default stays broken and forgetting the flag leaves the boundary
wide.

Restarted **before** `up`, not after, so `--wait` waits for health — `restart`
has none of its own, and reporting a lab up while its proxy is still coming
back is the same class of lie. `restart` rather than `--force-recreate`,
because recreating discards the container filesystem and would throw away
whatever an agent installed in the lab on an unrelated `sal up`.

**The skip list is a DENY-list of exactly two, and the criterion is the network
graph.** `observer` and `log-rotator` are on **neither** network on purpose, so
neither enforces anything and neither can be the one holding a stale boundary
file. Everything else running is restarted, including a service the stack adds
later that `sal` has never heard of — the restatement can only fall behind in
the safe direction. Skipping those two is not tidiness: `docker compose
restart` **reassigns a published host port the compose file left to Docker**,
and the observer's is left to Docker precisely so two labs cannot collide — so
restarting it for nothing would move the audit trail's URL on every `sal up`,
taking an open browser tab and any running `sal observer tail` with it. All of
it is measured in `tests/compose/run.sh` rather than assumed, including the
premise: that `up` does not re-read a changed mounted file.

**The compose project name is passed with `-p` on every invocation**, and
written to `.env` as `COMPOSE_PROJECT_NAME` as well. The template hardcodes
`name: secure-agent-lab`, which is right for a deployment somebody copies by
hand and wrong for a machine running one lab per project: without this, every
lab is the same compose project, `labs list` cannot tell them apart, and
`labs down` stops whichever lab that name currently resolves to. Precedence is
`-p` > `COMPOSE_PROJECT_NAME` > the file, verified against compose 5.1.4.

**An upgrade rewrites files, and DELETES the ones the new release dropped.****An upgrade rewrites files, and DELETES the ones the new release dropped.**
Repinning without rewriting is not an upgrade — that is the problem this repo
exists for. The deletion half carries its own risk and is easy to forget: a
cred-gateway config left behind keeps whitelisting a route the entry no longer
exposes, which is a widened boundary nothing would report. An entry keeps the
slot it was assigned, because the slot is the addon's filename prefix and
therefore its load order, and load order is a security property. And one
provider that cannot make the move refuses the WHOLE upgrade: half a deployment
on each of two releases is a boundary nobody can describe.

**`sal drift` asks about the PIN, not about the newest release.** "Is this lab
what it claims to be?" and "what would moving change?" are different questions,
and the second already has a command: `sal upgrade --dry-run`. Keeping them
apart is what lets `drift` be a CI step — it exits non-zero on any finding, and
a check that also fired every time upstream released would be one nobody could
leave switched on.

It compares against the recorded **commit**, not the tag, for the same reason
every other command does: a tag can move, and drift measured against a moved
tag is the tag's, not the deployment's.

**`sal drift` is stricter than `check-drift.sh` about a file nobody owns, and
only because it knows more.** That script calls an unrecognised file `custom`
and passes it, which is right for a deployment assembled by hand — there, a
file with no upstream counterpart is ordinary. In a sal-managed deployment
every boundary file arrived through `sal init` or `sal providers add` and was
written down, so one that no record accounts for arrived some other way. The
thing most likely to have put an extra `.conf` in `cred-gateway/` is the agent
the boundary exists to contain, so it is a finding and it fails the check.

The scan covers exactly the three bind-mounted directories. **`lab/` is
deliberately not one of them**: it is the operator's build context and its
`Dockerfile` is theirs by design, so judging its contents would report their
own file as an intrusion. A `lab_setup` fragment inside it is still *compared*,
because it arrives as an expected file like any other — comparing what we know
about and judging what we do not are different operations.

**The generated `compose.yaml` is compared too**, against a fresh render rather
than against a file in the release. It is the least contractual of the three
formats and it is still where the loopback-only observer publish, the
`internal: true` lab network and every mount live — so an edit there is a
change to the boundary that no other check in this repo would see.

**An entry you wrote yourself lives in `~/.config/secure-agent-lab/providers/`,
laid out exactly like the bank.** Outside the project for the same reason a
deployment is, and it matters more here rather than less: an entry is code that
runs behind the credential boundary once installed, so a scaffold in the
workspace is one the agent could edit before an operator installed it. The
layout is the bank's own so that what someone writes can be proposed to the
bank unchanged — and so `sal` reads it with the same code that reads the bank,
rather than a second path that could disagree about what an entry is.

**Trusting a source and installing from it are TWO ACTS.** `sal providers
source add owner/repo` says whose code may run behind this machine's credential
boundary; `sal providers add slack@acme` says nothing new. A fully-qualified
repository at install time would collapse them — re-deciding trust every time,
in a long string pasted out of a README — and would leave nowhere to answer
"which sources does this machine accept". That question is what
`providers source list` exists for, and it is the reason the registry exists at
all. Same shape as Claude's plugin marketplaces, for the same reason.

**A bare name NEVER resolves to a source.** It means the bank, or your own
providers directory, exactly as before. If a bare name searched added sources,
then adding one could silently change what an existing name installs — which is
the ambiguity the whole registry exists to prevent. So a third-party entry is
always `entry@source`, and there is no resolution order to reason about.

**The source's commit is recorded per entry**, in `installed.json`'s optional
`source_commit`. A ref is a moving pointer, so comparing a lab against `main`
would mean a branch that moved reads as the lab drifting — the same reason a
deployment records the stack's commit beside its tag. `sal drift` fetches the
source at the recorded commit and names it in the finding; a source that was
removed leaves the entry UNRESOLVED rather than being compared against the
bank, where a same-named entry would make a foreign file look correct.

**A private source uses whatever credential the operator already has**, and
this is NOT in tension with `sal secrets set` refusing a value in argv. That
rule was briefly read as "sal must never touch a credential", which is the
wrong reading and produced a deferral that made private repositories — the case
that matters most, since what an organisation most wants to share is what it
cannot publish — impossible for no benefit.

The rule is about what a credential DOES. A provider credential is stored on
disk, mounted into the broker and injected into the agent's traffic: it is the
thing the boundary exists to protect, and the reasons argv is unacceptable for
it are all about persistence and exposure to processes the agent can reach.
A token for reading a providers repository authenticates one HTTPS GET made by
the operator's own tool, on the operator's own machine, with the operator's own
authority. It is never stored, never mounted, and never crosses into the lab —
`git clone` would use the same credential. The boundary is around the lab, and
`sal` is host-side and outside it.

So: `GITHUB_TOKEN`, then `GH_TOKEN`, then `gh auth token`. Explicit before
ambient, because when both exist the environment variable is what the operator
typed for this run while the keychain is whoever they logged in as months ago.
The token goes on the request header and nowhere else — never into
`sources.json`, which is a list of names and repositories with none of the mode
discipline the secrets directory has, and never into a URL, where it would
reach proxy logs and any error that quotes it.

Two consequences worth stating. A private repository answers **404 exactly as a
missing one does**, so "check your spelling" is the wrong first guess and the
error says which credential was used or that none was found. And `sal drift`
reads sources too, so a CI step checking a lab with a private source needs a
token available or every one of its entries becomes an unresolved finding.

**`providers source remove` does not touch what was installed.** Untrusting a
source sounds like it should revoke what came from it, which is why the command
says it does not: reaching into every lab on the machine to delete files is not
something to do quietly, and `sal providers remove` in the lab is the honest
way.

**A name in both banks is REFUSED, never resolved.** Preferring the local copy
silently installs something other than the reviewed entry somebody asked for;
preferring the bank silently ignores the one they wrote. Both are the wrong
shape of surprise for a command that installs code behind a credential
boundary. The refusal is also forward-compatible: a naming scheme that
qualifies third-party entries by their source replaces it, rather than having
to undo it. `installed.json` records `source` for the same reason — the two
cannot be told apart afterwards, they are not equivalent (one was reviewed by
whoever maintains the bank and one was not), and `sal drift` compares each
against a different tree.

**The provider skeleton is FETCHED too, and `internal/scaffold` is gone.** It
was a copy of the stack's API living here — mitmproxy's hook signature, the
broker's `require("../audit")`, nginx's location syntax — with the same status
as the compose template and the same problem: that is the image's API, which
this repo does not version, so a skeleton here did not move when a deployment
repinned. An addon-API change needed a `sal` release, and a scaffold from an
old `sal` could not match the release a lab was pinned to.

Stack 1.12.0 ships `template/provider/<shape>/`, and `providers create` fetches
it at the pinned commit exactly like a bank entry — so what somebody scaffolds
is what that release runs, and the stack's own lint checks the skeleton as if
it were a real entry. The package was DELETED rather than ported, as intended.

**The placeholder is read from the skeleton's own `provider.json` `name`, never
hardcoded.** The token is the stack's to choose: it was proposed as
`__PROVIDER__` and shipped as `acme`, and a `sal` with either baked in renders
a broken entry the day the other is used — silently, because the result is
still valid JSON and valid Python. Reading it back makes a change over there a
change to data here. The fixture skeleton in `tests/fixtures/` spells it
differently from upstream on purpose, so a hardcoded token fails the suite.

Two case forms are substituted and only two. `acme` names files, routes and
hosts; `ACME_TOKEN_PATH` is an environment variable, and left behind it gives
an entry whose broker reads a variable the manifest never declares — installs
fine, finds no credential, says nothing. Title case is deliberately NOT a third:
Go treats `_` as a word character, so `strings.Title("__provider__")` returns
the token unchanged and the branch renamed the entry to `Telegraph`. A skeleton
that spells its placeholder in prose keeps the spelling, which is cosmetic;
renaming an entry to a name it does not have is not.

And the word `provider` never moves — `provider.json` is a fixed filename,
`"load_band": "provider"` is a schema enum value, and `provider=` is the audit
trail's field **name** rather than its contents. That falls out of substituting
only the placeholder, and is asserted anyway, because a blanket rename of the
word corrupted all four once.

**There is no `--template` flag yet.** A flag with one legal value is a promise
about a naming scheme nobody has designed. Templates arrive when shapes emerge
from actually writing providers — and they arrive as data in the stack repo,
not as code here.

**`providers remove` deletes what the RECORD says, and reports what merely
looks like it.** The asymmetry is the point: deleting an unrecorded file
because its name matches is how a removal takes out something somebody wrote by
hand, while leaving one costs a line of output. The file that matters most is
the cred-gateway config — left behind it keeps whitelisting a route whose
broker provider is gone, which is the same widened boundary `upgrade`'s stale
deletion exists to prevent and `drift` reports as STALE.

**An entry's egress is SEEDED, and only what it left uncommented.** From stack
1.13.0 a bank entry ships `allowlist` — the destinations it needs, in the
allowlist's own syntax, with anything optional commented out. `providers add`
writes the uncommented lines into the deployment's allowlist and prints them;
`providers remove` takes them back out; `upgrade` rewrites them from the new
release, so a host an entry stopped needing stops being permitted.

Seeding at all is a departure from "the allowlist is the operator's", and it
earns it on one specific ground: the entry's broker provider and proxy addon
already run behind the credential boundary, so installing it has already
extended more trust than *you may reach the host you say you need*. Refusing to
seed produced the failure this replaces — a provider that installs cleanly and
has every request denied. Deriving it from `hosts` instead is not an option and
not only because `hosts` is a different list: it carries no methods, and a line
with none defaults to `GET,HEAD,OPTIONS`, which reads as configured and blocks
every POST.

What keeps it safe is the line it does not cross. **A commented destination is
never written** — that is the vendor's suggestion, not the entry's requirement,
and turning it on is the operator's to type. And what sal writes lives in a
marked block, so everything outside every block belongs to the operator and is
never touched. Without the block, removal would have to choose between leaving
a destination permitted after its provider is gone and deleting a line somebody
wrote by hand — the same asymmetry `providers remove` already follows for
files, applied to the one control where a leftover is a widened boundary rather
than a stale file.

**`sal allowlist` puts the verbs on the FILE, not on the provider.** A
hand-added line belongs to no entry, so `providers egress` could never describe
half of what is in there — and `providers config` is ambiguous besides, since
the manifest already has a `config` field meaning values in `.env`.

`list` answers "what may this lab reach, and who decided each line", which is
unanswerable by eye once there are a few entries and a few hand-written lines:
the file gives no clue which is which. `reset` is the one with a job nothing
else has — a lab edited into a state nobody can explain, where the symptom is
an agent that cannot reach its vendor and the cause is three edits ago. What an
entry's block should hold is answerable exactly, because the entry declares it.
That is NOT `sal drift`, which asks whether the lab is what the release ships
and deliberately does not compare the allowlist at all.

`allow` writes outside every block, which is what makes the line the
operator's: it survives `providers remove` and no upgrade rewrites it. `deny`
REFUSES a destination an entry owns rather than deleting it — deleting would
work until the next `providers add`, `upgrade` or `reset` wrote that block
again, and a grant that reappears with nothing to explain it is worse than one
that was never removed. The honest answer is `providers remove`, and the error
says so.

An empty allowlist is reported as a finding rather than printed as an empty
list, because empty here means ENFORCING and denying everything — the opposite
of what an empty listing usually implies.

**A `lab_setup` fragment goes in `lab/setup.d/<name>.sh`, and from stack
1.14.0 something runs it.** Below that release there was nowhere to: the lab
mounted only the proxy CA and the workspace, its command was `sleep infinity`,
and a fragment sal installed sat in the build context unread — so the two bank
entries that ship one installed and did not work.

NOT `lab/` itself, which is the build context and holds `Dockerfile` and
`entrypoint.sh`: a `*.sh` glob there would run the entrypoint inside itself.
Written to `setup.d/` at every release, including below the line where nothing
reads it, because a path that varied by pin is one `drift` and `upgrade` would
both have to reason about — and a lab created below it then already has its
fragments in the right place when it upgrades.

`sal` creates that directory rather than letting Docker create the bind source,
which would make it root-owned and unwritable by any later `providers add` —
the same reason the secrets directory is created deliberately.

**`lab/entrypoint.sh` is the mechanism, so `upgrade` REWRITES it**, alongside
the compose file — a deployment carrying an old copy would silently stop
running fragments a newer release expects to run. It is also OPTIONAL below
1.14.0, since the template did not ship one: treating every template file as
required made `sal init --stack v1.13.1` fail on a file that release never had.

**`lab/Dockerfile` stays the operator's, and that is the migration trap.** A lab
created before 1.14.0 upgrades into a state where the entrypoint is present, the
fragments are in place, the compose file mounts them — and the image still never
runs any of it, because their Dockerfile has no `ENTRYPOINT`. Everything looks
installed and the provider does not work, which is the failure the mechanism was
added to end. sal cannot rewrite that file, so it warns and prints the four
lines to add.

**Removing a provider never deletes a credential.** Reinstalling the provider
undoes the removal; deleting a credential undoes nothing, and the two are
different decisions. The paths are printed so `rm` is one line away for someone
who meant both. There is no prompt on the removal itself: it narrows the
boundary, and a confirmation on a safe reversible action only teaches people to
clear prompts — the same rule `labs down` follows.

Its variables DO go, from `.env` and `lab.env`, and they come from the
manifest — never from the provider's name. An entry the bank no longer carries
still has its recorded files removed, and its variables are reported as left
behind rather than guessed at: deriving env keys from a provider name is
exactly the per-provider knowledge this repo has none of.

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
listing the agent can run. Same rule for anything that shells out. A *path* is
not a value and stdin is not an argv — see "A pipe is refused by DEFAULT"
below for where that line falls.

**Mount `secrets/`, never its parent.** The consolidated location is
`~/.config/secure-agent-lab/secrets/`, replacing the stack's current
`~/.config/agent-creds/`. That parent also holds every lab on the machine, and
none of that belongs in the broker. `0700` on the directory, `0600` on files.

**There is no migration from `~/.config/agent-creds/`, deliberately.** Nobody is
known to be running the stack yet, so the population it would serve is empty —
and moving credentials is the operation with the worst failure mode in this
repo. Someone who does have an old directory re-enters each credential with
`sal secrets set`, which is a better exercise of the command than a code path
tested against nothing. `config.LegacySecretsDir` stays as a named constant so
the old location can be *reported* without anything being moved.

**Never inspect a credential's contents to choose a destination.** The
destination comes out of the manifest — the `file` on the secret the bank entry
declares — and nothing else. This is the no-per-provider-code rule one level
further in, and it fails worse when broken: a wrong guess writes a credential
into the file the broker reads for a *different* one, which is silent at
install and surfaces much later as a rejected request nobody traces back.

The temptation is real, so it is worth naming what it looks like. Anthropic has
two genuinely different credentials: an OAuth token (`…oat01-`) tied to a Claude
subscription, sent as `Authorization: Bearer`, and an API key (`…api03-`) tied
to a Console org, sent as `x-api-key`. Different auth systems, different
revocation, different wire format — and `sal` could "just know" which prefix
means which file. It must not. An earlier draft of this document proposed
`--type oauth|api-key` for exactly that, which was vendor knowledge wearing a
flag's clothes.

**The bank already answers it, and better.** A provider with two credential
kinds declares two `secrets`, each with its own `env`, its own `file` and its
own `prompt`. Which one the operator meant is settled by *which prompt they
answered*, and recorded by *which file exists* — the broker reads
`ANTHROPIC_AUTH_TOKEN_PATH` first and falls back to `ANTHROPIC_API_KEY_PATH`,
so the filename **is** the record. Nothing is derived, so nothing can be
mis-derived. `sal secrets set anthropic` walks the array and prints the bank
author's own words, including their precedence rule, without this CLI having
heard of either credential.

`internal/invariants` holds the guard: no vendor credential shape may appear as
a string literal in this repo's source, listed in
`testdata/credential-shapes.txt`.

**What the rule does NOT forbid.** `sal` does read a credential — to write it to
a file — and `internal/secrets.ResolveFile` stats what the operator typed to ask
whether they meant a path. Neither picks a destination, and the second is
confirmed by a human before anything is copied. The line is *destination*, not
*contact*.

That confirmation earns its place on the failure it catches, which is the one
that is currently silent: someone pasting `~/Downloads/key.pem` into a prompt
that asked for a key. Without the check, `sal` writes thirty-four bytes of path
into the credential file and reports success. `--from-file` covers the same
ground non-interactively, and is not an exception to the never-in-argv rule — a
*path* in an argv reveals nothing, while a *value* in an argv is the whole
problem.

**A pipe is refused by DEFAULT and available behind `--from-stdin`.** This
document used to say a pipe is refused outright, "because a pipe is an argv one
process upstream". True of `echo $TOKEN | sal …` and false of every other
shape — `op read …`, `vault kv get`, `gpg -d` — and `sal` cannot tell them
apart, so it took the strict default. Right instinct, wrong width: it left no
way to take a credential from a secret manager without `op read … > /tmp/tok`
first, which puts plaintext on disk and depends on someone remembering the
`rm`. That is worse than what the refusal was protecting against.

The flag is what makes the difference, and not because it is safer —
`echo $TOKEN | sal … --from-stdin` is exactly as exposed as without it. It is
that **the absence of a terminal stays an error**. Cron, CI, a `make` recipe, a
stray `< /dev/null`: on a bare pipe every one of those silently becomes an input
source, and `sal secrets set <provider>` with no selector walks *several*
credentials, so the first would eat the stream and the rest read EOF. So
`--from-stdin` carries the same rule `--from-file` does — it sets exactly one
credential, named — and naming both flags is refused rather than resolved by
precedence. A value flag (`--value`, `--from-literal`, `--token`) remains
forbidden: stdin is not argv, and the two are different exposures.

**A credential is named by its `file`, exactly — never by its `env`.** The env
var is what the *broker* reads and its value is a path *inside the container*,
so `ANTHROPIC_AUTH_TOKEN_PATH` names neither the credential nor anywhere the
operator can put one; `anthropic-auth.token` is what they see in
`sal secrets list` and on disk. Because `Validate` refuses a manifest whose
secrets share a `file`, an exact match on it is unambiguous **by construction**
— which is the real prize, since it removes every tie-breaking path from the
one decision in this CLI that fails silently when it goes wrong. An exact `env`
name is still refused *with an explanation* rather than a "no such credential",
because someone who typed it read a deployment's `.env` and reasoned backwards.
No operator-facing message names a credential any other way — not the prompt,
not the listing, not the skip line.

**`multiline` picks the default, never the destination.** The manifest's
`multiline` flag exists to choose a prompt widget, and it does three jobs, all
consistent with that: it selects the paste-vs-path default, it decides whether
the read terminates on a blank line, and it decides whether the stored value is
trimmed. That last one is load-bearing — a token read out of a file carries the
newline an editor left, and passing it through produces `Authorization: Bearer
sk-…\n`, an invalid header that surfaces as a 401 long after anyone is watching.
A PEM's trailing newline is part of the file and must survive. Getting a default
wrong costs one keystroke; getting a destination wrong is unrecoverable, which
is why only the first is inferred.

**Adding a manifest field is a generation event, not an additive change.** The
schema is `additionalProperties: false` and this CLI implements that as
`json.Decoder.DisallowUnknownFields()`, so a bank entry carrying a new optional
field makes every older `sal` fail to decode it *at all* rather than ignore it.
The bank therefore cannot extend `secrets[]` — or anything else — without
bumping `schema_version`, which contract item 4 says should happen on the order
of never. The bar for a new field is that the CLI cannot do a correct job
without it. "The interactive prompt should know a value lives in a file" did not
clear it: `multiline` already discriminates well enough to pick a default.

## Command grammar

`gcloud`-shaped: `sal GROUP | COMMAND ...` — group(s), then verb, then
positional, with a small set of bare top-level commands, exactly as `gcloud`
keeps `init` and `info` alongside `gcloud storage`.

```
sal providers add cloudflare
sal providers create telegram --template rest-bearer
sal secrets set anthropic
sal secrets set anthropic anthropic-auth.token
sal secrets set github --from-file ~/Downloads/app.private-key.pem
op read op://vault/anthropic/token | sal secrets set anthropic anthropic-auth.token --from-stdin
sal secrets list
sal features list
sal features enable observer
sal features disable observer
sal observer open
sal observer tail
sal labs list
sal labs down api-3f2a1b0c
sal labs down --all
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

**Stdout is the URL; everything else is stderr.** That falls out of the rule
above rather than being a second decision — `--no-open` is then also how a
script takes the URL, with no format to parse and nothing to strip. Same
division in `tail`: the trail is on stdout and can go straight into `grep`,
while which URL is being tailed goes to stderr.

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
  never needs the Docker socket itself. `tests/compose/run.sh` is what
  makes "the CLI is the stable contract" a checked claim: it pins the
  behaviours sal's code assumes — profile selection from `.env` and from
  `--profile`, `config --profiles`, the `port` read-back and its failure shape,
  what `ps --quiet` answers for a service with no container — against the real
  binary. Every one of them was written from documentation first, and one was
  wrong: `docker compose rm` CAN remove a service whose profile is not enabled,
  as long as the service is named. sal passes `--profile` anyway, because that
  behaviour is compose's to change and nothing here should depend on it.
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
   `--help` listing commands that exit 1 is not a 1.0. **Done** — held by
   `internal/cli`'s `TestEveryCommandInTheGrammarIsImplemented`, whose
   allowlist of known stubs is empty. That test replaced the txtar script
   which asserted each unwritten command exited non-zero; every line in it was
   deleted by the change that implemented the command, and once the last one
   went there was nothing left for it to guard.
2. Both JSON formats declared stable at their current generation.
   `COMPATIBILITY.md` is that declaration, and both formats are now locked by
   tests that fail on a changed field set — `internal/deployment`'s
   `TestTheRecordFormatIsWhatIsPublished` and `internal/lab`'s
   `TestThePointerFormatIsWhatIsPublished`. What is left is time at generation
   1 without another field being wanted.
3. The `lab_setup` question resolved — fragments install today and nothing
   sources them, so `github` and `gcp` install without fully working.
4. The install script exercised against a real published release.
5. A written compatibility statement: this table, and what a major bump means.
   Written, at `COMPATIBILITY.md`, and published before 1.0 on purpose — the
   promises are cheaper to argue with while they can still change.

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

**The mitigations are only worth something if they REFUSE**, so `tests/install/`
produces each refusal rather than assuming it: a tampered archive, a release
with no `checksums.txt`, a signature that does not verify, an architecture
there is no build for. `curl` and `cosign` are fakes first on `PATH` — the same
technique the txtar scripts use for `docker` — so it needs no network and no
published release.

That harness pins `PATH` to its own fakes plus `/usr/bin:/bin`, and that is a
correctness property rather than hygiene: whether `install.sh` takes the cosign
branch is decided by whether cosign is ON `PATH`, so inheriting the developer's
would decide which half of the script is under test. On a machine with cosign
installed, the "signature NOT checked" path would never run at all. Not
hypothetical — it is how this harness's first run passed a test it was not
running.

**Signing produces a Sigstore BUNDLE, and verifying needs cosign v3.** Not a
preference: cosign v3 removed `--output-signature` and `--output-certificate`
from `sign-blob`, and removed `--signature` and `--certificate` from
`verify-blob`, so the previous detached `.sig` + `.pem` pair could no longer be
produced *or* checked. The release workflow pins the cosign version for the
same reason — the signing flags in `.goreleaser.yaml` and the verify flags in
`install.sh` are v3's, and a silent jump across that line breaks releases at
the one moment nobody is watching. `make snapshot` skips signing, because
keyless signing needs an OIDC token only CI has.

**Every 0.x release is marked a pre-release**, rather than left to GoReleaser's
`auto` — which only looks at the tag's suffix and would publish `v0.2.0` as a
full release. The README says the design is still settling and the two on-disk
formats are not declared stable; the release page must not say otherwise. This
becomes `auto` at 1.0.

## Still open

- **Whether `sal` should generate a `.devcontainer` for a project, and what it
  does with one that is already there.** Deliberately tabbed for its own
  release: nothing in the code integrates with VS Code today, so the question
  costs nothing to leave open. What `sal open` does in the meantime is settled
  and is under Non-obvious invariants — it opens the lab, and warns about a dev
  container that is not it.

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

**Where `sal` reads Docker rather than driving it, fake the `docker` binary.**
`sal labs list` asks `docker compose ls` which projects exist and which are up.
Its two findings that matter — a lab that is *running*, and a compose project
running out of the labs directory that no deployment there accounts for —
cannot be produced by starting real containers in a test, and the second is not
a state anything should be able to create on purpose. A `/bin/sh` script first
on `PATH` makes both deterministic, and exercises the real `exec` path and the
real JSON decode rather than a seam cut into the production code for testing.
Note this is the *reading* half only; the warning above still stands for
anything that starts containers.

The same technique extends past Docker. `observer tail` is tested against a
real SSE server on a real loopback port, started by the `observerd` testscript
command, which publishes its address for the fake `docker` to answer
`compose port observer 9000` with — so the path under test runs end to end,
from asking Docker for the port through the HTTP read to the formatted line.
`sal observer open`'s launcher is `$BROWSER` pointed at a script that records
its argv, which is honoured on every platform `sal` builds for and so needs no
per-OS fake.

**But testscript cannot see the one rule `observer open` exists for.** It
captures stdout and stderr separately, so a version that launched a browser
*first* and printed the URL afterwards passes every assertion a script can
make — while being precisely the bug the design is shaped around. That
ordering is checked by a unit test giving both streams one buffer, which is
what an operator's terminal actually is. Worth remembering as a general
limit rather than a fact about this command: when the property is *relative
order across the two streams*, txtar is the wrong layer.

**A container is right for the install script, and wrong for the lab.** Testing
`curl … | bash` across `debian:slim`, `alpine` and `ubuntu`, as root and
non-root, catches what actually breaks — arch detection, busybox `tar`,
`sha256sum` vs `shasum`, which PATH directory is writable — and needs no Docker
inside the container.

Both halves of that exist. `tests/install/run.sh` is the LOGIC — the refusals,
the resolution, what it says — and runs anywhere, with `curl` and `cosign`
faked on `PATH` and `HOME` pointed at scratch so a run cannot reach the
operator's own `sal`. `tests/install/containers.sh` runs that same file inside
`debian:stable-slim`, `ubuntu:24.04` and `bash:5` (Alpine-based, so busybox
`tar` and busybox `sha256sum`), as root and as an ordinary user, with
`--network none` and the checkout mounted read-only.

It found what it was built to find: `--ignore-missing` and `--status` are GNU
spellings that busybox does not have, and `-s` is a *shasum* spelling that GNU
`sha256sum` does not have. So `install.sh` now uses `-c` alone, throws the
output away rather than asking for quiet, and picks the checksum line for its
own archive with `awk` instead of passing `--ignore-missing`. That last change
also turns "the checksums file has no line for this archive" into an explicit
refusal rather than something inferred from a checker's exit status.

Note what the container tier does NOT do: no Docker socket is mounted and
nothing starts a lab. It runs a shell script against a filesystem, which is why
a container is right here and wrong for the lifecycle — see the warning below.

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
