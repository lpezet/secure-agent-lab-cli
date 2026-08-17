# Fixture skeleton addon. `provider=` below is the audit trail's field NAME and
# must survive the rename; only the placeholder token moves.
import hostmatch

HOSTS = ("api.boilerplate.invalid",)


def request(flow) -> None:
    if flow.response is not None:
        return
    if not hostmatch.matches(flow.request.host, HOSTS):
        return
    audit(provider="boilerplate", event="cred_injected")
