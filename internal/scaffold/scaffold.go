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
// The manifest declares ONE route and does not expose it, which is the whole
// of the static-key shape: a long-lived secret is a reusable secret, and the
// stack's own rule is that a route handing one over stays unexposed. A
// skeleton shipping an exposed route would teach the mistake `providers add`
// refuses. A provider that can mint something short-lived and scoped is a
// different shape, and gets a different skeleton.
func manifestBody(name, env string) string {
	return `{
  "schema_version": 1,
  "name": "` + name + `",
  "summary": "TODO: one line — which credential, for which hosts, and what the lab gets",
  "min_stack": "1.7.0",
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
    { "path": "/` + name + `/cred", "exposed": false }
  ]
}
`
}

// Substitution is deliberate rather than a blanket rename of the word
// "provider", and the difference is not cosmetic. That word appears in this
// text as an English noun ("your provider"), as a literal filename
// (provider.json), as a schema enum value (load_band) and as the KEYWORD of an
// audit field (provider="x"). Renaming all of them produces prose that reads
// like nonsense and an audit call whose field is named after the vendor —
// which changes the shape of the trail rather than its contents.
func brokerBody(name, env string) string {
	return `// Broker side of the static-key shape: hold the long-lived secret, hand it to
// the proxy, and never to the lab. Reachable only from the proxy on the
// ` + "`secure`" + ` network — cred-gateway does not whitelist this path, which is what
// ` + "`" + `"exposed": false` + "`" + ` in provider.json declares.
const fs = require("fs");
const { logEvent } = require("../audit");

// Returns the file's contents, or null — never "". An empty credential file is
// absent as far as callers are concerned, and collapsing it here keeps that
// true for a caller that tests ` + "`" + `!== null` + "`" + ` rather than truthiness.
function tryReadFile(path) {
  if (!path) return null;
  try {
    const value = fs.readFileSync(path, "utf8").trim();
    return value || null;
  } catch (err) {
    if (err.code === "ENOENT") return null;
    throw err;
  }
}

module.exports = {
  // The route keys are the export, not a "routes" object around them: the
  // broker does Object.assign(routes, require(file)) over every provider file.
  //
  // Read fresh on every call rather than cached: it is a local file read, and
  // it means rotating the credential needs no broker restart. The env var is
  // the one provider.json declares under secrets[].env, and its value is a
  // path INSIDE the container — never read a credential from anywhere else.
  "/` + name + `/cred": async (url, send) => {
    const token = tryReadFile(process.env.` + env + `_TOKEN_PATH);
    if (!token) {
      console.error("[broker] no credential file at ` + env + `_TOKEN_PATH");
      logEvent("cred_unavailable", { provider: "` + name + `" });
      return send(500, { error: "no ` + name + ` credential configured" });
    }

    // The event records the SHAPE of what happened. Never the value, and never
    // anything derived from it: observer serves this trail over HTTP.
    console.log("[broker] issued ` + name + ` credential to proxy");
    logEvent("cred_issued", { provider: "` + name + `", cred_type: "static_key" });
    send(200, { type: "static_key", value: token });
  },
};
`
}

func proxyBody(name string) string {
	return `"""Inject a static key for this provider's hosts, and log what was called.

The static-key shape: the broker holds a long-lived secret, this attaches it to
requests leaving the lab, and the lab never holds it. If your provider can mint
something short-lived and scoped instead, prefer that — the stack's exposure
rule is about exactly that difference.

No NNN_ prefix on this file: the installer assigns one from the manifest's
band when it lands in a deployment.
"""
import requests
from cachetools import TTLCache
from mitmproxy import ctx, http

import audit
import hostmatch

BROKER_URL = "http://broker:8080"

# TODO: the hosts this provider authenticates to. They must agree EXACTLY with
# ` + "`hosts`" + ` in provider.json, both directions: a host declared there and not
# matched here means the credential is silently never injected, and a host
# matched here but not declared there is an injection the egress allowlist was
# never seeded for.
HOSTS = ("api.` + name + `.invalid",)

_cache = TTLCache(maxsize=1, ttl=300)


def _get_cred() -> str:
    """Return the credential from the broker, cached for five minutes."""
    if "cred" not in _cache:
        r = requests.get(f"{BROKER_URL}/` + name + `/cred", timeout=5)
        r.raise_for_status()
        _cache["cred"] = r.json()["value"]
    return _cache["cred"]


def _endpoint(flow: http.HTTPFlow) -> str:
    """A loggable identifier for what was called, never the raw path.

    The query string is split off FIRST and only the leading path segments are
    kept. Both halves matter: flow.request.path includes the query, so an
    ?access_token= would land in the audit trail that observer serves over
    HTTP — and keeping two segments bounds the cardinality.

    TODO: if your provider carries its credential IN the path — Telegram's
    /bot<TOKEN>/<method> is the example that has actually shipped — drop the
    segments that hold it. Which slice is safe is provider-specific, which is
    why there is no shared helper for it.
    """
    parts = [p for p in flow.request.path.split("?", 1)[0].split("/") if p][:2]
    return "/" + "/".join(parts)


def request(flow: http.HTTPFlow) -> None:
    # Do not act on a request an earlier addon has already refused. mitmproxy
    # calls every addon's request hook regardless, so without this an addon
    # after a denial still runs: overwriting the refusal message, or — worse —
    # fetching a credential from the broker and logging cred_injected for a
    # request that never leaves. Deliberately dependency-free, because
    # deployments vendor this file at pins that may predate any shared helper.
    if flow.response is not None:
        return

    # flow.request.host is the real destination. Do NOT use pretty_host: it
    # prefers the client-supplied Host header, so the lab container could point
    # a request at its own server, spoof the header, and have the real
    # credential injected into a request that never goes to the vendor.
    #
    # hostmatch normalises case, a trailing root dot and a :port before
    # comparing. A plain == against a lowercase name is what let
    # http://BROKER:8080/… through the internal-host block until stack 1.9.2.
    if not hostmatch.matches(flow.request.host, HOSTS):
        return

    # Stripped BEFORE the credential is fetched, not after. _get_cred() raises
    # when the broker is unreachable, so fetching first means the strip never
    # runs and the agent's own header goes to the vendor untouched — the
    # opposite of what the strip is for.
    #
    # TODO: name every header this provider authenticates with.
    for header in ("Authorization", "X-Api-Key"):
        if header in flow.request.headers:
            del flow.request.headers[header]

    # TODO: attach it the way this provider expects.
    flow.request.headers["Authorization"] = f"Bearer {_get_cred()}"

    ctx.log.info(f"` + name + `: injected credential for {flow.request.method} {_endpoint(flow)}")
    audit.log_event(
        "cred_injected",
        provider="` + name + `",
        method=flow.request.method,
        endpoint=_endpoint(flow),
    )
`
}
