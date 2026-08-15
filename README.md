# sal

A CLI over [secure-agent-lab](https://github.com/lpezet/secure-agent-lab) — the
Docker stack that runs autonomous agents without exposing long-lived
credentials to the agent's process.

> **Status: early, and pre-1.0 honestly.** `init`, `up`, `down`, `upgrade`,
> `drift`, `open`, `providers add|list`, `secrets set|list`, `labs list|down`
> and `observer open|tail` work against a real stack. `providers create|remove`
> and `features` are still stubs that exit non-zero —
> deliberately, since a stub returning 0 would let a script believe a
> credential was stored.

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
curl -fsSL https://raw.githubusercontent.com/lpezet/secure-agent-lab-cli/main/install.sh | bash
curl -fsSL https://raw.githubusercontent.com/lpezet/secure-agent-lab-cli/main/install.sh | bash -s v0.1.0
```

The version there pins **the binary, not the lab**. The stack release a project
runs is pinned per-project and moved by `sal upgrade`. Keeping those on separate
lines is deliberate: if the install command also pinned the stack, upgrading
your CLI would silently move everyone's security boundary.

Piping a script into a shell to install a security tool is a look, so: the
script verifies the checksum **before** extracting anything, verifies the
cosign signature over it when cosign is on PATH and says so plainly when it is
not, and is short enough to read first. Clone-and-inspect is a supported path,
not a footnote.

```
git clone https://github.com/lpezet/secure-agent-lab-cli
cd secure-agent-lab-cli && make build     # -> bin/sal
```

## Commands

```
sal init                     create a lab in this directory
sal up | down                act on the lab in this directory
sal open                     a shell in the lab container, behind the proxy
sal upgrade                  repin to a newer stack release AND update the files it owns
sal drift                    report files that differ from the pinned release

sal providers add NAME       install a bank entry
sal providers create NAME    scaffold a new one
sal secrets set PROVIDER     store a credential, read from the terminal with echo off
sal secrets list             what is stored, what is loose, what nothing claims
sal features enable NAME     lifecycle verbs, uniform across every feature
sal observer open            print the audit trail's URL, then try a browser
sal observer tail            stream the audit trail to a terminal with no browser
sal labs list                what is running on this machine with credentials attached
sal labs down NAME | --all   stop one from anywhere, or every one of them
```

## Two things worth knowing

**`sal` is a host-side tool and never ships inside the lab image.** Not an
ergonomic choice: `sal secrets set` and `sal providers add` widen the boundary,
so a `sal` on `PATH` inside the lab would hand the agent a supported interface
for widening its own allowlist.

**A forgotten lab is not idle.** It is a live credential-injecting proxy with
the secrets directory mounted. That is what `sal labs list` is for, and
`sal labs down` is how you stop one whose project no longer exists — the case
`sal down` cannot reach, because it finds a lab from a project.

**`sal drift` is the check for the problem above.** It compares every file the
deployment owns against the release it is pinned to — proxy addons, broker
providers, gateway configs and the generated compose file — and exits non-zero
on any difference, so it can be a CI step. Because a sal-managed deployment
records what was installed, it also reports the two things a file-by-file diff
cannot: a file the release ships that this lab does not have, and a file in a
managed directory that nothing installed.

**The URL comes before the browser, always.** `sal observer open` prints the
observer's URL on stdout and only then attempts to launch something. A launch
fails silently over SSH, in WSL and inside a dev container, so printing
afterwards would lose the one useful output exactly where it is hardest to get
back. `--no-open` prints it and attempts nothing, which is also how a script
gets it: stdout is the URL and nothing else. For a terminal with no browser at
all, `sal observer tail` streams the same trail as text.

**`sal` knows nothing about any provider, including its credentials.**
`sal secrets set anthropic` asks for an OAuth token and then an API key, in that
precedence, using the bank author's own wording — because the manifest declares
both, each with its own destination file. No flag names a vendor's credential
kinds and no code reads a value's prefix to decide where it goes; a wrong guess
there would file a credential under the wrong one silently. `internal/invariants`
fails the build if a vendor credential shape ever appears in this repo's source.

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
