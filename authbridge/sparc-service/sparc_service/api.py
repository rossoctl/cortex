"""FastAPI application for the SPARC reflection service.

Endpoints:
  POST /reflect  — run SPARC on a proposed tool call, return the verdict.
  GET  /healthz  — liveness (always ok if the process is up).
  GET  /readyz   — readiness (config valid and component buildable).
"""

from __future__ import annotations

import json
import logging
import os

from fastapi import FastAPI, HTTPException
from fastapi.concurrency import run_in_threadpool

from .engine import ReflectionEngine
from .models import ReflectRequest, ReflectResponse
from .settings import Settings

log = logging.getLogger(__name__)

# When SPARC_LOG_REQUESTS=true, log the full incoming ReflectRequest JSON so
# you can inspect exactly what the caller sends (useful for diagnosing
# unexpected tool argument keys). Disabled by default — payloads can be large.
_LOG_REQUESTS = os.getenv("SPARC_LOG_REQUESTS", "").strip().lower() in {"1", "true", "yes"}

# When SPARC_STRIP_TOOL_ARG_KEYS is set (comma-separated key names), those keys
# are removed from every tool_calls[].function.arguments JSON object before the
# request reaches SPARC. Use to drop agent-injected keys that are not in the
# tool spec and would cause SPARC to reject the call.
# Example: SPARC_STRIP_TOOL_ARG_KEYS=session_id,request_id
_STRIP_KEYS: frozenset[str] = frozenset(
    k.strip() for k in os.getenv("SPARC_STRIP_TOOL_ARG_KEYS", "").split(",") if k.strip()
)


def _strip_tool_arg_keys(tool_calls: list[dict], keys: frozenset[str]) -> list[dict]:
    """Return a copy of tool_calls with the named argument keys removed."""
    result = []
    for tc in tool_calls:
        fn = tc.get("function", {})
        raw_args = fn.get("arguments", "")
        try:
            args = json.loads(raw_args) if isinstance(raw_args, str) else raw_args
            if isinstance(args, dict):
                args = {k: v for k, v in args.items() if k not in keys}
            new_args = json.dumps(args) if isinstance(args, dict) else raw_args
        except (json.JSONDecodeError, TypeError):
            new_args = raw_args
        result.append({**tc, "function": {**fn, "arguments": new_args}})
    return result


def create_app(engine: ReflectionEngine | None = None) -> FastAPI:
    """Build the FastAPI app. Inject ``engine`` in tests; defaults to env config."""
    settings = engine.settings if engine is not None else Settings.from_env()
    engine = engine or ReflectionEngine(settings)

    app = FastAPI(
        title="Rossoctl SPARC reflection service",
        version="0.1.0",
        summary="In-process SPARC pre-tool reflection over HTTP.",
    )
    app.state.engine = engine
    app.state.settings = settings

    @app.get("/healthz")
    def healthz() -> dict[str, object]:
        return {
            "status": "ok",
            "provider": settings.provider,
            "model": settings.model,
            "track": settings.track,
        }

    @app.get("/readyz")
    def readyz() -> dict[str, object]:
        ok, detail = engine.ready()
        if not ok:
            raise HTTPException(status_code=503, detail={"status": "not_ready", "reason": detail})
        return {"status": "ready", "provider": settings.provider, "model": settings.model}

    @app.post("/reflect", response_model=ReflectResponse)
    async def reflect(request: ReflectRequest) -> ReflectResponse:
        if _LOG_REQUESTS:
            log.info("incoming reflect request: %s", request.model_dump_json())

        if _STRIP_KEYS and request.tool_calls:
            request = request.model_copy(
                update={"tool_calls": _strip_tool_arg_keys(request.tool_calls, _STRIP_KEYS)}
            )
            if _LOG_REQUESTS:
                log.info("after strip (%s): tool_calls=%s", sorted(_STRIP_KEYS), request.tool_calls)

        # SPARCReflectionComponent.process is synchronous (and CPU/IO bound on the
        # LLM call); run it off the event loop so the service stays responsive.
        try:
            return await run_in_threadpool(engine.reflect, request)
        except ValueError as exc:  # bad input (e.g. unsupported track) → 400
            raise HTTPException(status_code=400, detail={"error": str(exc)}) from exc
        except Exception:
            # Reflection failure → 502 so the plugin can apply its fail policy.
            # Keep the full error in the service logs; never echo provider
            # exception text to the caller — it can embed endpoints/credentials.
            log.exception("reflection failed")
            raise HTTPException(status_code=502, detail={"error": "reflection failed"}) from None

    return app


app = create_app()
