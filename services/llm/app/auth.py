"""Current-user resolution for the llm service.

The llm is NOT browser-exposed on the personas/chat path — it sits behind the gateway, which verifies
the session cookie and forwards the resolved id as the ``X-User-Id`` header. So there is no crypto to
do here: the llm simply TRUSTS that header as the current user id. An empty value ("") means a guest.

This mirrors the Go services' local cookie verification, minus the crypto, because the gateway has
already done the verifying for this hop.
"""
from __future__ import annotations

from fastapi import Request


def current_user_id(request: Request) -> str:
    """The gateway-verified user id from X-User-Id ("" = guest)."""
    return (request.headers.get("X-User-Id") or "").strip()
