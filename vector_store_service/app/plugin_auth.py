"""Optional bearer-token check for plugin-runner images.

The plugin platform starts a runner with PLUGIN_AUTH_TOKEN set and sends that
token on every request (except GET /health). When the variable is unset -- the
normal, non-plugin deployment -- nothing is installed and behaviour is unchanged.
This is defence in depth: the runner already sits on a private network reachable
only through the controller.
"""

from __future__ import annotations

import hmac
import os

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse


def install_plugin_auth(app: FastAPI, token: str | None = None) -> bool:
    token = os.getenv("PLUGIN_AUTH_TOKEN", "") if token is None else token
    if not token:
        return False
    expected = "Bearer " + token

    @app.middleware("http")
    async def _require_token(request: Request, call_next):
        if request.url.path != "/health":
            supplied = request.headers.get("authorization", "")
            if not hmac.compare_digest(supplied.encode(), expected.encode()):
                return JSONResponse({"error": {"code": "unauthorized", "message": "bad credentials"}}, status_code=401)
        return await call_next(request)

    return True
