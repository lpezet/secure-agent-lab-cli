# Run Claude Code behind the boundary

A walkthrough with a destination: by the end you have Claude Code running
inside a lab, reaching Anthropic through a proxy that holds the credential the
agent never sees, over an allowlist you wrote, with every request in an audit
trail.

Two halves. The first builds that — install `sal`, create a lab, give it a
credential path, put Claude Code in the image, open the egress it needs, start
it, work in it, watch the trail. The second is keeping it honest afterwards:
checking it is still what it claims to be, moving it to a newer release, and
taking it down.

Run it against a throwaway project first. The credential is the only thing you
cannot fake — steps 1 to 3 work without one, and Claude itself will not.

**Prerequisites.** Docker with the compose plugin, network access on the first
run (the stack is fetched from GitHub, and five images are built), a project
directory, and an Anthropic credential. `sal` never needs the Docker socket
itself — it shells out to `docker compose`.

> **What is verified and what is not.** Steps 1–4, 10, and the dry runs in 12
> and 13 were run end to end while writing this, against stack v1.12.0, and
> their output below is real — steps 0, 1, 2 and 10 with a published binary
> rather than a local build. The steps that start containers — 6 through 9, 11
> and 14 — have since been run against real containers on stack v1.14.0, which
> is what found the `PATH` line in step 5: everything installed, and `sal open
> claude` exited 127. Their transcripts below are still the fake-`docker` ones,
> so expect the sense to be right and a message or two to differ.

---

## 0. Get the binary

```bash
curl -fsSL https://raw.githubusercontent.com/lpezet/secure-agent-lab-cli/main/install.sh | bash
```

or from a checkout, which is also how you get an unreleased change:

```bash
git clone https://github.com/lpezet/secure-agent-lab-cli
cd secure-agent-lab-cli && make build     # -> bin/sal
```

```bash
sal --version
```

```
sal          0.1.0 (8bfda02f705f)
stack        unknown — this directory has no lab
```

Two versions, always. `sal` and the stack are tagged on separate lines: a
single number could not answer "how far behind is my security boundary?".

(A binary from `make build` reports `v0.1.0-3-g8bfda02` instead — `git
describe` keeps the `v` and the commit distance, GoReleaser strips both. Same
build, different provenance.)

---

## 1. Create the lab

```bash
cd ~/projects/my-agent-project
sal init
```

```
pinning to stack v1.12.0, the newest release this sal knows about; pass --stack to choose another
lab      my-agent-project-082ce7bb
at       ~/.config/secure-agent-lab/labs/my-agent-project-082ce7bb
stack    v1.12.0 (f7431786d093)
project  ~/projects/my-agent-project
wiring   compose.yaml, fetched at v1.12.0
addons   carried by the proxy image at v1.12.0, not vendored here

The egress allowlist is enforcing and empty, so the lab can reach nothing yet.
Add the destinations your agent needs to …/allowlist — one per line, `domain [METHODS]`.
Deleting that file permits everything instead, with a warning at startup.

Next: `sal providers add <name>` to give it a credential path, then `sal up`.
The first `sal up` builds five images from the stack repo and takes a few minutes.
```

**What just happened, and where.** Two trees, not one:

```
my-agent-project/.sal/lab.json                     a committable pointer: name + stack tag
~/.config/secure-agent-lab/labs/<name>/            the deployment itself, 0700
```

The deployment is deliberately **outside** your project. The agent works in the
project, so a deployment kept there is one the agent can edit — and the proxy
addons, broker providers and gateway configs are exactly what it would want to
edit. Out of the workspace they are not merely unwritable, they are invisible.

`<name>` is `<basename>-<8 hex of the project's absolute path>`. The suffix is
load-bearing: two projects called `api` must not resolve to one lab.

Commit `.sal/lab.json`. A colleague who clones and runs `sal init` gets a lab
pinned to the same release.

To pin somewhere else: `sal init --stack v1.12.0`. To work from a local
checkout of the stack instead of downloading — an air-gapped machine, or an
unreleased branch — add `--stack-dir ../secure-agent-lab` to this and any later
command.

---

## 2. See what the bank offers

```bash
sal providers list --available
```

```
NAME        MIN STACK  SUMMARY
anthropic   1.1.0      Anthropic credential for api.anthropic.com; prefers OAuth token over API key, Admin API blocked
cloudflare  1.1.0      Scoped Cloudflare API token minted per profile from a long-lived minter token
gcp         1.7.0      Short-lived GCP access token for one service account, by impersonation (no key) or from a key file
github      1.1.0      GitHub App installation token for api.github.com, plus git credential-helper and identity endpoints

bank at stack v1.12.0 (f7431786d093)
```

At **this lab's pinned release**, not the newest one. Offering entries from a
newer bank would mean offering ones whose `min_stack` this lab does not
satisfy, and that failure lands at runtime inside a container rather than here.

The lab you just created has none of them: the broker answers 404 on every
credential route and cred-gateway denies everything but `/healthz`.

---

## 3. Install one

Look before you write:

```bash
sal providers add anthropic --dry-run
```

```
anthropic — Anthropic credential for api.anthropic.com; prefers OAuth token over API key, Admin API blocked
slot     010 (provider band)
hosts    api.anthropic.com
write    broker/anthropic.js
write    proxy/010_anthropic.py
route    /anthropic/cred — not exposed to the lab

dry run: every check passed and nothing was written
```

Three things in that output are worth reading rather than skimming:

- **`slot 010`** is the addon's filename prefix, and the prefix is its load
  order. The bank never bakes a number in; `sal` assigns the lowest free slot
  in the manifest's band (`policy` 000–009, `provider` 010–899, `post` 900+).
- **`hosts`** is what the addon matches and what the manifest declares, and
  they have to agree in both directions.
- **`not exposed to the lab`** means no cred-gateway config is written for that
  route. `/anthropic/cred` hands over a reusable secret, so the lab must never
  reach it — the proxy injects it into requests instead. An entry whose
  manifest marked it exposed would be refused.

Then, for real:

```bash
sal providers add anthropic
```

It prompts, so run it in a terminal. It walks the credentials the manifest
declares, in the manifest's own precedence, using the bank author's own words:

```
Anthropic OAuth token (preferred; leave blank to use an API key)
  stored as anthropic-auth.token — paste the value, or the path to a file holding it
>
Anthropic API key (used only if no OAuth token is set)
  stored as anthropic.key — paste the value, or the path to a file holding it
>
skipped anthropic.key (optional)

installed anthropic into my-agent-project-082ce7bb
Run `sal up` to restart the lab against it — the broker, proxy and
cred-gateway read these files at startup, so a running lab has not picked them up.
```

Blank is fine at both prompts; step 4 covers setting one later. Note what `sal`
did **not** do: it never inspected a value to decide where it goes. The
destination is the `file` the manifest declares for the credential you were
being asked for, and nothing else. Anthropic has two genuinely different
credentials and `sal` could "just know" which prefix means which — it must not,
because a wrong guess writes a credential into the file the broker reads for
the other one, which is silent now and surfaces as a rejected request nobody
traces back.

The deployment now looks like this:

```
.env                        broker and proxy environment, incl. ANTHROPIC_*_PATH
lab.env                     the LAB container's environment only — ANTHROPIC_API_KEY=proxy-injected
allowlist                   egress policy, enforcing and empty
compose.yaml                the stack's own template, fetched at v1.12.0, verbatim
lab/Dockerfile              the one image you build yourself
broker/anthropic.js
proxy/010_anthropic.py
.sal/installed.json         what was written, at which commit
```

Two env files, not one, and that is a boundary property: the lab must never
receive the broker's environment.

---

## 4. Store the credential

```bash
sal secrets set anthropic
```

The value is read from the terminal **with echo off**. There is no flag that
takes a value and there will not be one — an argv is in shell history, in `ps`,
and in any process listing the agent can run. Three shapes are supported, and
they are three different exposures:

```bash
sal secrets set anthropic                                    # prompt, echo off
sal secrets set anthropic anthropic-auth.token               # just that one
sal secrets set github --from-file ~/Downloads/app.pem       # a PATH in argv reveals nothing
op read op://vault/anthropic/token | \
  sal secrets set anthropic anthropic-auth.token --from-stdin
```

A pipe with **neither** flag is refused. `--from-stdin` is for a secret manager
with no file to point at; requiring it explicitly is what keeps a lost terminal
in cron or CI an error rather than an input source.

A credential is named by its **file** — `anthropic-auth.token` — never by the
environment variable the manifest pairs it with. That variable is what the
*broker* reads and its value is a path inside the container, so it names
neither the credential nor anywhere you can put one.

```bash
sal secrets list
```

```
~/.config/secure-agent-lab/secrets

anthropic
  anthropic-auth.token  set — 0600, modified 2026-08-16 03:57
                        Anthropic OAuth token (preferred; leave blank to use an API key)
  anthropic.key         not set (optional)
                        Anthropic API key (used only if no OAuth token is set)
```

It never prints a value, a length or a fingerprint. It also reports files in
the secrets directory that no installed provider claims — an unclaimed
credential is a live secret mounted into every broker on this machine that
nothing references.

Credentials are **shared by every lab on this machine**, so overwriting one
rotates it for all of them. That is stated and confirmed, never assumed:

```
anthropic-auth.token is already set (modified 2026-08-16 03:57)
left anthropic-auth.token alone
```

---

## 5. Put Claude Code in the image

The lab image is **the one thing in the deployment that is yours**. `sal
upgrade` never overwrites it and `sal drift` never judges its contents — it is
your build context, so a tool that reported your own Dockerfile as an intrusion
would be wrong.

```bash
$EDITOR ~/.config/secure-agent-lab/labs/my-agent-project-082ce7bb/lab/Dockerfile
```

The template ships a Debian-based image with Node already on it, and a comment
marking where to add things. Append:

```dockerfile
# Last, so busting this layer does not redo apt. Accepts "latest" (default),
# "stable", or an exact version. Change the value to force a re-download.
ARG CLAUDE_VERSION=latest
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- "${CLAUDE_VERSION}"

# The installer puts it in ~/.local/bin, which the image's PATH does not carry.
ENV PATH="/root/.local/bin:${PATH}"
```

**That `ENV` line is not optional, and the failure it prevents looks like a
`sal` bug.** The installer lands `claude` in `$HOME/.local/bin`, and the base
image adds that directory from `/etc/bash.bashrc` — an *interactive shell*
file. So `sal open` finds it and `sal open claude` does not:

```
OCI runtime exec failed: exec: "claude": executable file not found in $PATH
sal: docker compose exec lab claude: exit status 127
```

`sal open <command>` is `docker compose exec lab <command>` — a bare exec with
no shell between, which is what makes it honest: it runs what you named, not
what a shell resolved on your behalf. The cost is that it sees the image's
`ENV PATH` and nothing else. Anything you install to a directory outside that
PATH needs saying so in the image, which is where it belongs anyway — a tool
findable only from an interactive prompt is not really installed.

**Build-time network is the HOST's, not the lab's.** The allowlist governs the
running container's egress; `docker build` goes out through your machine. So
that `curl` works even though the lab can currently reach nothing — and it
means the image build is *not* in the audit trail. That is a real property
worth being conscious of rather than a gap: what you bake into the image is
outside the boundary, and the boundary begins when the container starts.

TLS through the proxy is already handled. The template sets
`NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `SSL_CERT_FILE` and
`GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` to the proxy's CA, plus `HTTP_PROXY` and
`HTTPS_PROXY` in **both** cases — gRPC reads only the lowercase ones, and with
the uppercase alone a gRPC client ignores the proxy entirely.

For a fuller example — a non-root `agent` user, an entrypoint, a plugin
installer — read `examples/claude-code/` in the stack repo. Note that it keeps
the container alive with `sleep infinity` and attaches with `exec`, because
Claude Code misbehaves as a detached container entrypoint.

---

## 6. Open the egress allowlist

The lab starts able to reach **nothing**. That is the right default and a
surprising one, so `init` says so.

You do not have to work out what Anthropic needs, because the entry declared
it: `providers add` in step 3 already wrote the destinations it requires, and
printed them. Check who decided what:

```bash
sal allowlist list
```

```
anthropic
  api.anthropic.com       GET,POST
  platform.claude.com     GET
```

Both are required, and the second is the one nobody would guess: Claude Code
calls `platform.claude.com/v1/oauth/hello` before it will run and treats
failing it as fatal. It is in the allowlist and deliberately **not** in the
entry's `hosts` — it answers unauthenticated, so it is a host the agent must
*reach*, not one your credential is attached to. Reachable and credentialed are
two different lists that happen to overlap.

Only what the entry left **uncommented** is written. Its optional destinations
— telemetry, error reporting — are the vendor's suggestion and stay off until
you turn them on, which is yours to type.

Add what else *your* agent needs, which belongs to you rather than to any
entry:

```bash
sal allowlist allow api.github.com GET,POST,PATCH,PUT,DELETE
sal allowlist allow github.com '*'
```

Those land outside every managed block, which is what makes them yours: they
survive `sal providers remove`, and no upgrade rewrites them. `sal allowlist
list` groups them under `yours`. You can still edit the file directly —
`~/.config/secure-agent-lab/labs/my-agent-project-082ce7bb/allowlist`, one
entry per line, `domain [METHODS]` — and anything outside a marked block is
never touched.

Matching is on label boundaries: `*.example.com` covers `a.example.com` and
`a.b.example.com`, never `example.com` itself and never `evilexample.com`.
METHODS defaults to `GET,HEAD,OPTIONS` — safe reads only. **The file being
present is what makes the allowlist enforcing**; deleting it permits every
destination, with a warning at startup.

Expect `blocked` lines in the trail even when everything works. Claude Code
also reaches telemetry and error-reporting hosts, which are not on the
allowlist and should not be — they are denied, and the denials are recorded.
That is the boundary doing its job, not a misconfiguration. To quiet them, add
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` to the lab's `lab.env`; a key you
add there by hand survives `providers add` and `upgrade`, which only touch the
ones a manifest names.

---

## 7. Start it

```bash
sal up --build
```

The first run builds five images from the stack repo and takes a few minutes.
After that, `sal up`.

Six services come up: `broker`, `proxy`, `cred-gateway`, `observer`,
`log-rotator`, `lab`. Two networks — `secure` and `lab`, the latter
`internal: true`, so the proxy is the only way out. `observer` and
`log-rotator` are on **neither**, on purpose: they reach the shared audit
volume without becoming a channel between the two sides.

`sal up` is also what you run after changing anything in the deployment — a
provider added or removed, a line allowed or denied. Every one of those files
is mounted into a container that read it **at startup**, so `docker compose up`
on its own would find nothing to do and leave the boundary as it was. `sal up`
restarts what was already running and says which:

```
restarted broker, cred-gateway, lab, proxy
          they read the deployment's files at startup, so `up` alone
          would have left them holding what was there before
```

`observer` and `log-rotator` are not in that list, for the same reason they are
on no network: they enforce nothing, so neither can be the one holding a stale
boundary file — and restarting the observer would move the audit trail's URL,
since Docker assigns that port.

```bash
sal labs list
```

```
~/.config/secure-agent-lab/labs

my-agent-project-082ce7bb
  status    running
  project   ~/projects/my-agent-project
  stack     v1.12.0 (f7431786d093)
  providers anthropic

1 lab on this machine, 1 running.
```

---

## 8. Watch what the agent does

```bash
sal observer open
```

Prints the URL on **stdout first**, then attempts a browser. That order is the
whole design: a launch fails silently over SSH, in WSL and inside a dev
container, so printing afterwards would lose the one useful output exactly
where it is hardest to get back. `--no-open` prints it and launches nothing,
which is also how a script gets it — stdout is the URL and nothing else.

The host port is never chosen by `sal`. The observer publishes as
`127.0.0.1::9000` — empty host port, Docker assigns — and the assignment is
read back with `docker compose port`. Collisions between labs are structurally
impossible rather than tracked in a lockfile, and the loopback prefix stays:
the trail is served over plain HTTP with no auth and is only safe for not being
reachable off the host.

For a terminal with no browser at all:

```bash
sal observer tail
sal observer tail --follow=false | grep cred_injected
```

Events are rendered **by shape**, never by provider — the timestamp, service
and event name every writer in the stack emits, then whatever else the line
carried, as `key=value`. Nothing is dropped, including a line that is not JSON.
A formatter that gave one vendor's events their own layout would be vendor
knowledge, and would silently hide fields it did not recognise.

Before the lab is up, both say so rather than printing a URL that would fail to
connect:

```
sal: lab "my-agent-project-082ce7bb" has no observer port published, which means it is not running.
Start it with `sal up`, or `sal labs list` to see what is running on this machine
```

---

## 9. Run Claude Code inside it

This is the destination.

```bash
sal open claude                # Claude Code, behind the boundary
```

`sal open` takes a command, so this execs `claude` directly rather than
dropping you at a shell first — which is why step 5 puts `~/.local/bin` on the
image's `PATH`. If this exits 127 while `sal open` then `claude` works, that
line is what is missing. With no arguments you get a shell instead:

```bash
sal open                       # bash in the lab container
sal open python -c 'import urllib.request; print(1)'
```

Nothing about `claude` is special to `sal` — it is your image, your command.
There is deliberately no `sal claude`: the CLI knows nothing about any vendor,
which is the same rule that stops it having a flag for Anthropic's two
credential kinds. A shell alias is the shortcut, and it costs nothing:

```bash
alias salc='sal open claude'
```

**What is different about running it here.** Claude has no credential. Your
token is in a file the broker reads, on a network the lab is not on; the proxy
injects it into requests to `api.anthropic.com` on the way past. `lab.env` sets
`ANTHROPIC_API_KEY=proxy-injected`, which is a placeholder — a real-looking
value the agent can read and nothing can be done with. Read it out of the
process, exfiltrate it, print it in a log: it is worth nothing anywhere.

Your project is mounted at `/workspace`, and it is the one mount the agent can
write. `HTTP_PROXY`/`HTTPS_PROXY` (and their lowercase forms, which is what
gRPC reads) point at the proxy, and the proxy's CA is trusted — which is how a
Node tool talks through an intercepting proxy at all.

`sal open` opens the **lab**. If a dev container is running for the same
project, it is a warning rather than a redirect: a dev container you brought
yourself is not on the `lab` network, so it does not go out through the proxy
and nothing it does reaches the audit trail. Opening it because it happened to
be there would hand you a shell you believe is inside the boundary when it is
not.

Now put the two windows side by side. Ask Claude to do something that reaches
the network, and watch step 8's `sal observer tail` record the request going
out with a credential it never handled:

```
2026-08-16T20:41:02Z  proxy   cred_injected  provider=anthropic host=api.anthropic.com
```

Then ask it for something off the allowlist and watch the other half work:

```
2026-08-16T20:41:19Z  proxy   blocked        reason=allowlist host=example.com method=GET
```

Those two lines are the whole thesis of the stack: the agent reached what you
permitted, with a credential it never held, and the attempt to go elsewhere is
both refused and recorded.

---

## 10. Check it is still what it claims to be

```bash
sal drift
```

```
deployment  ~/.config/secure-agent-lab/labs/my-agent-project-082ce7bb
pinned      v1.12.0 (f7431786d093)
project     ~/projects/my-agent-project

ok       broker/anthropic.js            matches bank/anthropic/broker/
ok       proxy/010_anthropic.py         matches bank/anthropic/proxy/
ok       compose.yaml                   matches the template at v1.12.0

summary     0 drift · 0 missing · 0 stale · 0 unowned

This lab is what it says it is: every file it owns matches v1.12.0.
That is a known list, not a review — it cannot see a way of being unsafe
nobody has thought of.
```

**This is the check for the problem the CLI exists to fix.** The compose file
builds every image from the pinned tag, but the files that *enforce* the
boundary — `proxy/*.py`, `broker/*.js`, `cred-gateway/*.conf` — are
bind-mounted from the deployment's own directories. Repinning moves the images
and leaves those files exactly as they were, with nothing in `docker compose
ps` to show for it.

To see it fire, edit an addon and run it again:

```bash
echo "# edited" >> ~/.config/secure-agent-lab/labs/my-agent-project-082ce7bb/proxy/010_anthropic.py
sal drift ; echo "exit=$?"
```

```
DRIFT    proxy/010_anthropic.py         differs from bank/anthropic/proxy/

summary     1 drift · 0 missing · 0 stale · 0 unowned

DRIFT and MISSING are what `sal upgrade` rewrites. Running it against the
same release (v1.12.0) reinstalls every file from the release itself.
sal: this lab is not what v1.12.0 ships
exit=1
```

`--show-diff` prints what differs, not just which files.

It exits **non-zero on any finding**, so it can be a CI step. Four findings it
reports that a file-by-file diff cannot:

| | |
|---|---|
| DRIFT | the file differs from the release |
| MISSING | the release ships it and this lab does not have it |
| STALE | the record says it was installed and the release no longer ships it |
| UNOWNED | it is in a managed directory and nothing installed it |

The last one is stricter than the stack's own `check-drift.sh`, and only
because `sal` knows more: in a `sal`-managed deployment every boundary file
arrived through `init` or `providers add` and was written down, so one no
record accounts for arrived some other way — and the thing most likely to have
put an extra `.conf` in `cred-gateway/` is the agent the boundary exists to
contain.

It compares against the release the lab is **pinned to**, so it answers "is
this lab what it claims to be?". "What would moving change?" is a different
question, and it has its own command — step 11.

`lab/` is deliberately not scanned: it is your build context and its
`Dockerfile` is yours by design.

---

## 11. Turn a feature off and on

```bash
sal features list
```

```
observer     on       running
```

Two columns because they can disagree: what `.env` says, and what Docker says.

```bash
sal features disable observer
sal features enable observer
```

A feature **is** a compose profile, and its service has the same name — that
equivalence is the whole implementation. Which features are on is a value in
the deployment's `.env` (`COMPOSE_PROFILES`), the variable compose reads
itself, so a `docker compose up` run by hand starts what `sal up` starts.

Disabling stops the service **before** it records the change; enabling records
before it starts. Both orders point the same way: the state that must never be
reachable is a record saying a feature is on while nothing is running it,
because that is how someone comes to trust an audit trail that does not exist.

Disabling never touches a volume — the trail the observer was serving outlives
the observer.

Lifecycle verbs live in `features`, uniformly, for every feature. `sal observer
disable` is deliberately **not** a command; if each feature owned a copy of
enable/disable/list there would be no single place to answer "what is on?".

---

## 12. Move to a newer release

```bash
sal upgrade --dry-run
sal upgrade                        # or --to v1.13.0
```

**This is the reason the CLI exists.** Repinning without rewriting is not an
upgrade. So it reinstalls every recorded provider from the new release, keeps
the slot each was assigned (the slot is load order, and load order is a
security property), re-fetches the wiring, and **deletes files the new release
no longer ships**.

That deletion half is easy to forget and carries its own risk: a cred-gateway
config left behind keeps whitelisting a route the entry no longer exposes,
which is a widened boundary nothing would report.

Every provider is checked before anything is written, and one that cannot make
the move refuses the **whole** upgrade. Half a deployment on each of two
releases is a boundary nobody can describe.

```bash
sal up --build                     # the images move with the pin
sal drift                          # and confirm
```

---

## 13. Remove a provider

```bash
sal providers remove anthropic --dry-run
```

```
anthropic — slot 010
delete   broker/anthropic.js
delete   proxy/010_anthropic.py
unset    ANTHROPIC_API_KEY_PATH
unset    ANTHROPIC_AUTH_TOKEN_PATH

dry run: nothing was removed
```

It deletes what the **record** says, and reports what merely looks like it. The
asymmetry is the point: deleting an unrecorded file because its name matches is
how a removal takes out something you wrote by hand, while leaving one costs a
line of output.

The variables come from the manifest, never from the provider's name. And
**the credential is not deleted** — reinstalling the provider undoes the
removal, deleting a credential undoes nothing, and the two are different
decisions. The path is printed so `rm` is one line away if you meant both.

No confirmation prompt: this narrows the boundary, and a prompt on a safe
reversible action only teaches people to clear prompts.

---

## 14. Stop

```bash
sal down                    # containers go; the audit trail and proxy CA survive
sal down --volumes          # and the trail and CA go too
```

From anywhere on the machine, for a lab whose project you deleted — the case
`sal down` cannot reach, because it finds a lab *from* a project:

```bash
sal labs list
sal labs down my-agent-project-082ce7bb
sal labs down --all
```

Names are spelled exactly, for the same reason a credential is: two projects
called `api` are what the hash suffix exists to keep apart, so a prefix would
be ambiguous precisely where it matters. `--all` or names, never both and never
neither — a bare `sal labs down` that stopped everything would be a
machine-wide action nobody typed.

There is deliberately **no `--volumes` on `labs down`**. That flag deletes a
lab's audit trail, and across a whole machine that is not an operation with a
safe shape: deleting one trail is a decision, deleting every trail is one
decision applied to things you were not thinking about. `sal down --volumes`
per project makes you visit each one.

One lab that will not stop does not abandon the rest — aborting would leave
*more* of them running, which is the opposite of what was asked for. The exit
status is still non-zero.

**A forgotten lab is not idle.** It is a live credential-injecting proxy with
the secrets directory mounted. `sal labs list` is the answer to "what is
currently running with my credentials attached", which makes it a control
rather than a convenience.

---

## Appendix: an entry the bank does not carry

```bash
sal providers create telegraph
```

Scaffolds a bank entry in `~/.config/secure-agent-lab/providers/telegraph/`,
laid out exactly like the bank — outside your project, because an entry is code
that runs behind the credential boundary once installed, so a scaffold in the
workspace is one the agent could edit before you installed it.

```bash
$EDITOR ~/.config/secure-agent-lab/providers/telegraph/proxy/telegraph.py
sal providers add telegraph
```

`sal` reads it with the same code that reads the bank, and `installed.json`
records `source` so the two can be told apart afterwards — they are not
equivalent, since one was reviewed by whoever maintains the bank and one was
not. A name that exists in **both** banks is refused rather than resolved:
preferring either one silently installs something other than what you asked
for.

The generation constraints for writing a provider from scratch live in the
stack repo's `PLAYBOOK.md`, which covers exactly the case a bank of finished
entries cannot. This command scaffolds; it does not replace reading that.

---

## Appendix: the short version

```bash
sal init                                  # create the lab, outside the project
sal providers list --available            # what the bank offers at this pin
sal providers add anthropic               # install the credential path
sal secrets set anthropic                 # store the credential, echo off
$EDITOR <lab>/lab/Dockerfile              # add Claude Code — the image is yours
$EDITOR <lab>/allowlist                   # api.anthropic.com POST; egress starts closed
sal up --build                            # six services
sal observer open                         # the audit trail, in a second window
sal open claude                           # THE POINT: Claude, behind the boundary

sal drift                                 # is it still what it claims to be?
sal upgrade                               # move the pin AND the files it ships
sal features list                         # what is on, and what is running
sal labs list                             # what is running with credentials attached
sal down                                  # trail survives; --volumes deletes it
```
