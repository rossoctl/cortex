"""Env-gated activation hook for the propagate-only OTel shim.

Dockerfile.otel-shim installs this as ``_lineage_propagate.py`` in the app
environment's site-packages next to a one-line ``.pth``, so ``site`` imports
it at every interpreter start, before any app code. The shim thus attaches
through the environment (like ``JAVA_TOOL_OPTIONS`` / ``NODE_OPTIONS``); the
container's command is never rewritten.

Inert unless ``LINEAGE_PROPAGATE=1``. When active: pin the propagate-only
posture as env *defaults* (every exporter ``none``, ``tracecontext,baggage``
propagators — ``setdefault``, so a deliberate override still wins), then run
stock auto-instrumentation via ``initialize()``; which instrumentors activate
depends on what the app imports.

Failure policy: never take the app down. ``initialize()`` swallows its own
exceptions, the guard below covers the rest, and ``site`` itself survives a
broken ``.pth`` line. A hook failure therefore means propagation is OFF and the
trace fragments at this pod, visibly (``parent.source=none`` on its outbound
hops) — absent lineage,
never wrong lineage.
"""

import os

if os.environ.get("LINEAGE_PROPAGATE") == "1":
    os.environ.setdefault("OTEL_TRACES_EXPORTER", "none")
    os.environ.setdefault("OTEL_METRICS_EXPORTER", "none")
    os.environ.setdefault("OTEL_LOGS_EXPORTER", "none")
    os.environ.setdefault("OTEL_PROPAGATORS", "tracecontext,baggage")
    try:
        from opentelemetry.instrumentation.auto_instrumentation import initialize

        initialize()
    # Deliberately broad: never break the app this hook rides in.
    except Exception:  # noqa: BLE001
        import logging

        logging.getLogger(__name__).exception(
            "lineage propagate hook failed to initialize; propagation is OFF for this process"
        )
