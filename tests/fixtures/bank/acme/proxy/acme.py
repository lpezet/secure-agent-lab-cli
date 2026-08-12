"""Fixture proxy addon.

No NNN prefix, deliberately. The bank never bakes a slot number in; the
installer assigns the lowest free one in the manifest's band.

The hostname literals below must agree exactly with the manifest's `hosts`, in
both directions — a host the addon matches but the manifest omits is egress
nobody declared, and a host the manifest declares but the addon ignores is a
credential that silently never gets injected.
"""

import audit

HOSTS = ("api.acme.example", "uploads.acme.example")


def request(flow):
    if flow.request.pretty_host in HOSTS:
        flow.request.headers["authorization"] = "Bearer <injected by fixture>"
