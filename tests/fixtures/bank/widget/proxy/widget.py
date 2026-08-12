"""Fixture proxy addon, minimal.

This entry ships no cred-gateway config at all, because it exposes nothing.
That is the shape a proxy-injection-only provider takes: the broker mints, the
proxy injects on the way out, and the lab is never handed anything it could
replay.
"""

import audit

HOSTS = ("api.widget.example",)


def request(flow):
    if flow.request.pretty_host in HOSTS:
        flow.request.headers["x-api-key"] = "<injected by fixture>"
