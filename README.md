# sal

A CLI over [secure-agent-lab](https://github.com/lpezet/secure-agent-lab) — the
Docker stack that runs autonomous agents without exposing long-lived
credentials to the agent's process.

> **Status: early.** The command tree, the manifest reader and the invariant
> tests are in place; the commands themselves are stubs that exit non-zero.

## What it is for

The stack versions the security boundary by tag. But a deployment holds its own
copies of `proxy/*.py`, `broker/*.js` and `cred-gateway/*.conf` — they are
bind-mounted, so they do **not** move when the tag does. A lab can repin to a
release containing a security fix and keep running the vulnerable file, because
the fix landed in a file it owns a copy of.

A tool that *installed* those files is a tool that can *update* them. That is
what `sal` is for; everything else it does is secondary.

## Install

```
curl -fsSL https://github.com/lpezet/secure-agent-lab-cli/install | bash
curl -fsSL https://github.com/lpezet/secure-agent-lab-cli/install | bash -s "v1.2.0"
```

The version there pins **the binary, not the lab**. The stack release a project
runs is pinned per-project and moved by `sal upgrade`. Keeping those on separate
lines is deliberate: if the install command also pinned the stack, upgrading
your CLI would silently move everyone's security boundary.

Piping a script into a shell to install a security tool is a look, so:
clone-and-inspect is a supported path, every release ships `checksums.txt`
signed with cosign, and the install script is short enough to read first.

```
git clone https://github.com/lpezet/secure-agent-lab-cli
cd secure-agent-lab-cli && make build     # -> bin/sal
```

## Commands

```
sal init                     create a lab in this directory
sal up | down | open         act on the lab in this directory
sal upgrade                  repin to a newer stack release AND update the files it owns
sal drift                    report files that differ from the pinned release

sal providers add NAME       install a bank entry
sal providers create NAME    scaffold a new one
sal secrets set NAME         store a credential, read from the terminal with echo off
sal features enable NAME     lifecycle verbs, uniform across every feature
sal observer open | tail     read the audit trail
sal labs list                what is running on this machine with credentials attached
```

## Two things worth knowing

**`sal` is a host-side tool and never ships inside the lab image.** Not an
ergonomic choice: `sal secrets set` and `sal providers add` widen the boundary,
so a `sal` on `PATH` inside the lab would hand the agent a supported interface
for widening its own allowlist.

**A forgotten lab is not idle.** It is a live credential-injecting proxy with
the secrets directory mounted. That is what `sal labs list` is for.

## Development

```
make check    # go vet + go test
make build    # bin/sal
```

Three layers, none of which needs Docker yet:

- **`internal/invariants`** is the test that matters most: it fails if any bank
  entry name appears as a string literal in this repo's Go source. The bank is
  data and `sal` is a generic installer over it — the moment that test fails,
  the CLI has learned about a specific provider and the two repos are coupled
  again.
- **`cmd/sal/testdata/script/*.txtar`** tests the CLI as its users meet it —
  exit status, stdout versus stderr, the file tree left behind. It is where the
  command grammar is observable: a unit test cannot tell you that
  `sal observer disable` is not a command.
- **`tests/fixtures/`** is a fake bank under invented provider names, plus a set
  of manifests that must be refused. See its README for what each one traps.

See `CLAUDE.md` for the decisions behind all of this and the reasoning that
produced them.
