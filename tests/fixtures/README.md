# Fixtures

A fake bank, and a set of manifests that must be refused.

Everything here uses **invented provider names** — `acme`, `widget`, `leaky`.
That is not decoration. `internal/invariants` fails if a real bank entry name
appears as a string literal in this repo's Go source, and a fixture named after
a real provider is one careless copy-paste away from becoming the per-provider
code that test exists to prevent. Invented names keep the fixture honest: if a
test passes with `acme`, it passes because the installer is generic.

The layout mirrors a real bank entry exactly, because the point of a fixture is
to be indistinguishable from the thing it stands in for:

```
bank/<name>/
  provider.json                  the manifest
  broker/<name>.js          →    /app/providers/<name>.js
  proxy/<name>.py           →    /addons/NNN_<name>.py     ← installer assigns NNN
  cred-gateway/<name>.conf  →    /etc/nginx/gateway.d/<name>.conf
  lab/setup.sh                   optional fragment
```

## `local-stack/` — a stand-in for a checkout of the stack repo

Laid out the way the stack repo is, so `--stack-dir` can be pointed at it:
`bank/` holds the entries, `stack/proxy/addons/` holds the proxy addons every
deployment gets at `sal init`. That flag replaced an `--offline` flag and a
cache; a directory you can read is a better test seam than hidden state.

### `local-stack/bank/` — entries that must install cleanly

| Entry | What it covers |
|---|---|
| `acme` | Every optional field populated: two secrets (one multiline, one optional), two config values (one with a default), three routes of which one is **not** exposed, `lab_env`, `lab_setup`, and all four files. |
| `widget` | Required fields only. One route, not exposed — so it carries **no cred-gateway config at all**, which is the shape a proxy-injection-only provider takes. |

`acme` sets `min_stack` to an older release and `widget` to a newer one, so a
test can pin a lab between them and watch exactly one of the two be refused.

## `refused/` — manifests an installer must reject

Each is a whole entry, not a snippet, because the failure has to be reachable
through the same path a real install takes.

| Entry | Why it must be refused |
|---|---|
| `future-schema` | `schema_version` above what this build supports. Refused rather than best-effort: what it declares may be a control. |
| `unknown-field` | Carries `audit_mode`, a field this build has never heard of. It looks exactly like a control, which is the point — `additionalProperties: false` is what stops an installer quietly ignoring it. |
| `missing-exposed` | A route that does not say whether it is exposed. Absent must not read as "not whitelisted, fine". |
| `stack-too-new` | `min_stack` above any plausible deployment. The failure this catches is silent at install and fatal at runtime. |
| `host-mismatch` | Declares a host its addon never quotes. The credential is then never injected and nothing errors — the request goes out bare, or the vendor rejects it much later and somewhere else. |
| `leaky-conf` | A manifest marking `/leaky/token` as `exposed: false`, and a cred-gateway config that whitelists it anyway. Not a manifest error — a whole-entry one, and the only fixture here that a JSON check alone will never catch. |

`leaky-conf` is the fixture worth keeping longest. Exposing a token route hands
the lab a reusable secret, and it is the one failure in this directory that
looks entirely fine field by field.
