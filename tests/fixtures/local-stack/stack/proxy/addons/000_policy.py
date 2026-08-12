"""Fixture stand-in for the stack's policy addon.

The real one blocks the proxy from forwarding to internal hostnames, which is
what stops the lab reaching the broker and walking around the cred-gateway
whitelist. What matters for a fixture is that init installs it at all.
"""

_INTERNAL_HOSTS = {"broker", "cred-gateway"}
