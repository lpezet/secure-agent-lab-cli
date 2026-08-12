"""Fixture addon that matches only ONE of the two declared hosts.

The manifest declares api.ghost.example and uploads.ghost.example. This addon
mentions the first and not the second, so a request to uploads never has a
credential injected — and nothing fails. The request simply goes out
unauthenticated, or the API rejects it much later and somewhere else.

That silence is why this is a refusal at install rather than a warning.
"""

import audit

HOSTS = ("api.ghost.example",)


def request(flow):
    if flow.request.pretty_host in HOSTS:
        flow.request.headers["authorization"] = "Bearer <injected by fixture>"
