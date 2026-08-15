// Package scaffold writes the skeleton of a bank entry.
//
// ⚠️ The templates here describe the STACK's API — mitmproxy's addon hooks,
// the broker's `require("../audit")`, nginx's location syntax — and this repo
// does not version that API. They are a copy that does not move when a
// deployment repins, which is the drift problem this whole repo exists to fix,
// appearing one level up.
//
// They are here anyway, for now, because the stack repo has no template to
// fetch yet. The intended end state is `bank/_template/` over there, fetched
// at the pinned tag exactly like a bank entry, at which point everything below
// is deleted rather than ported. Keep it in one file and keep it dumb, so that
// removal is a deletion and not a refactor. Two consequences while it lives
// here: an addon-API change needs a `sal` release, and a scaffold produced by
// an old `sal` may not match the release a lab is pinned to.
//
// Nothing here knows any provider. Every name in the output is the one the
// operator typed, and the placeholder host is under .invalid, a TLD reserved
// by RFC 2606 precisely so it cannot resolve to anyone's API.
package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// namePattern mirrors the schema's `name` and the bank's own entry-name rule.
// Applied before the name reaches a path, so a scaffold cannot be talked into
// writing outside the directory it was given.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// File is one scaffolded file.
type File struct {
	Path string // relative to the entry directory
	Mode os.FileMode
	Body string
}

// ErrExists means the entry directory is already there.
type ErrExists struct{ Dir string }

func (e *ErrExists) Error() string {
	return e.Dir + " already exists"
}

// Files returns everything the skeleton contains, in the order it should be
// reported. The manifest comes first because it is the file the others must
// agree with.
func Files(name string) ([]File, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("%q is not a usable entry name: lowercase letters, digits and hyphens, starting with a letter", name)
	}
	env := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))

	return []File{
		{Path: "provider.json", Mode: 0o600, Body: manifestBody(name, env)},
		{Path: filepath.Join("broker", name+".js"), Mode: 0o600, Body: brokerBody(name, env)},
		{Path: filepath.Join("proxy", name+".py"), Mode: 0o600, Body: proxyBody(name)},
		{Path: filepath.Join("cred-gateway", name+".conf"), Mode: 0o600, Body: gatewayBody(name)},
	}, nil
}

// Write creates the entry directory and everything in it.
//
// Refuses an existing directory rather than merging into it: a half-scaffolded
// entry mixed with someone's edits is a worse thing to hand back than an
// error, and this is the one command whose whole output is a starting point.
func Write(dir, name string) ([]File, error) {
	files, err := Files(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil, &ErrExists{Dir: dir}
	}

	// 0700 throughout, like every other directory sal owns: what is in here
	// decides which code runs behind the credential boundary.
	for _, f := range files {
		full := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(f.Body), f.Mode); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// The manifest declares one route that is NOT exposed and one that is, because
// that pair is the shape of the whole design: the lab may ask for a scoped,
// short-lived credential and may never reach the route that mints it.
func manifestBody(name, env string) string {
	return `{
  "schema_version": 1,
  "name": "` + name + `",
  "summary": "TODO: one line on what this provider gives the lab",
  "min_stack": "1.9.0",
  "load_band": "provider",
  "hosts": ["api.` + name + `.invalid"],
  "secrets": [
    {
      "env": "` + env + `_TOKEN_PATH",
      "file": "` + name + `.token",
      "prompt": "TODO: what to paste, in the words the operator will recognise"
    }
  ],
  "broker_routes": [
    { "path": "/` + name + `/token", "exposed": false },
    { "path": "/` + name + `/credential", "exposed": true }
  ]
}
`
}

func brokerBody(name, env string) string {
	return `// ` + name + ` — broker provider. Runs on the secure network, holds the
// long-lived credential, and hands out only what the lab is allowed to have.
//
// TODO: everything below is a skeleton. Read PLAYBOOK.md in the stack repo
// before trusting it — it covers writing a provider from scratch, which is the
// case a bank of finished entries cannot.
const fs = require("fs");
const { logEvent } = require("../audit");

// The PATH comes from the manifest's secrets[].env, and its value is a path
// INSIDE the container. Never read a credential from anywhere else, and never
// log its value — the audit trail records the shape of what happened.
const TOKEN_PATH = process.env.` + env + `_TOKEN_PATH;

function readToken() {
  return fs.readFileSync(TOKEN_PATH, "utf8").trim();
}

module.exports = {
  routes: {
    // exposed:false in the manifest, so cred-gateway must NOT whitelist this.
    // It is where the long-lived credential is used, and a lab that could
    // reach it would hold a reusable secret.
    "/` + name + `/token": async (req, res) => {
      logEvent("token_minted", { provider: "` + name + `" });
      res.end(JSON.stringify({ token: readToken() }));
    },

    // exposed:true, so this is what the lab actually calls. TODO: exchange the
    // long-lived credential for something scoped and short-lived here.
    "/` + name + `/credential": async (req, res) => {
      logEvent("credential_issued", { provider: "` + name + `" });
      res.end(JSON.stringify({ credential: "TODO" }));
    },
  },
};
`
}

func proxyBody(name string) string {
	return `"""` + name + ` — proxy addon.

Injects credentials into requests leaving the lab for this provider's hosts,
so the agent's process never holds one.

TODO: a skeleton. No NNN prefix here, deliberately: the bank never bakes a slot
number in, and the installer assigns the lowest free one in the manifest's band.
"""

import audit
from mitmproxy import http

# Must agree EXACTLY with the manifest's hosts, both directions: a host
# declared there and not matched here means the credential is simply never
# injected, and nothing errors at runtime to say so.
HOSTS = ("api.` + name + `.invalid",)


def request(flow: http.HTTPFlow) -> None:
    if flow.request.pretty_host not in HOSTS:
        return

    # TODO: fetch from the broker and set the header this API expects. The
    # broker is reachable from the proxy and NOT from the lab.
    audit.log_event("credential_injected", provider="` + name + `", host=flow.request.pretty_host)
`
}

func gatewayBody(name string) string {
	return `# ` + name + ` — cred-gateway whitelist.
#
# ONLY routes the manifest marks exposed:true may appear in this file. A route
# marked exposed:false that is whitelisted here hands the lab a reusable
# secret, which is the one mistake in a bank entry that cannot be seen by
# reading the manifest alone. ` + "`sal providers add`" + ` refuses an entry that does it.

location = /` + name + `/credential {
  proxy_pass http://broker:8080;
}
`
}
