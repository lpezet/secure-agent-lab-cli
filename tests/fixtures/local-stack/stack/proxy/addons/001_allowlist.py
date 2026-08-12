"""Fixture stand-in for the stack's egress allowlist addon.

Inert without /etc/agent-allowlist — the real one permits every destination and
warns at startup when the file is absent, which is why init installs it
unconditionally rather than holding an opinion about which addons matter.
"""
