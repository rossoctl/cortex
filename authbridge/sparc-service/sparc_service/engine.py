"""The reflection engine: lazily builds SPARC components and maps their output.

Components are built lazily and cached *per track* so the service starts even
when the LLM backend is briefly unavailable, switches tracks per request without
rebuilding the common case, and lets unit tests inject a fake component factory
(no network, no credentials).
"""

from __future__ import annotations

import logging
import threading
from dataclasses import replace
from typing import Any, Callable

from .models import ReflectionIssue, ReflectRequest, ReflectResponse
from .providers import build_component
from .settings import SUPPORTED_TRACKS, Settings

log = logging.getLogger(__name__)

# A factory takes a track name and returns an object exposing
# ``process(run_input, phase) -> output`` where output has
# ``reflection_result``, ``execution_time_ms`` and ``raw_pipeline_result``.
# The real one is altk's SPARCReflectionComponent.
ComponentFactory = Callable[[str], Any]


def _decision_str(decision: Any) -> str:
    """Normalize SPARC's decision enum/str to a plain string."""
    return str(getattr(decision, "value", decision))


def _extract_overall_score(raw_pipeline: dict[str, Any] | None) -> float | None:
    if not raw_pipeline:
        return None
    value = raw_pipeline.get("overall_avg_score")
    return float(value) if isinstance(value, (int, float)) else None


class ReflectionEngine:
    """Builds (per track) and invokes SPARC components, mapping IO to wire models."""

    def __init__(
        self,
        settings: Settings,
        component_factory: ComponentFactory | None = None,
    ) -> None:
        self._settings = settings
        self._factory: ComponentFactory = component_factory or (
            lambda track: build_component(replace(settings, track=track))
        )
        self._lock = threading.Lock()
        # Components keyed by track, so a per-request track override doesn't
        # rebuild the common case. Most deployments use exactly one track.
        self._components: dict[str, Any] = {}

    @property
    def settings(self) -> Settings:
        return self._settings

    def _component_or_build(self, track: str) -> Any:
        with self._lock:
            existing = self._components.get(track)
            if existing is not None:
                return existing
            component = self._factory(track)  # may raise → surfaced as 503 / not-ready
            self._components[track] = component
            return component

    def ready(self) -> tuple[bool, str | None]:
        """Best-effort readiness: try to build the default-track component once.

        Our own validation errors (missing creds, etc.) are safe to surface
        verbatim. A build exception may carry provider text (endpoints, request
        details), so it's logged in full and reported as a generic reason.
        """
        if self._settings.errors:
            return False, "; ".join(self._settings.errors)
        try:
            self._component_or_build(self._settings.track)
            return True, None
        except Exception:
            log.exception("SPARC component build failed")
            return False, "component initialization failed; see service logs"

    def reflect(self, request: ReflectRequest) -> ReflectResponse:
        """Run SPARC reflection on a proposed tool call and project the verdict."""
        from altk.core.toolkit import AgentPhase
        from altk.pre_tool.core import SPARCReflectionRunInput

        track = request.track or self._settings.track
        if track not in SUPPORTED_TRACKS:
            raise ValueError(f"unsupported track {track!r}; expected one of {sorted(SUPPORTED_TRACKS)}")

        component = self._component_or_build(track)

        run_input = SPARCReflectionRunInput(
            messages=request.messages,
            tool_specs=request.tool_specs,
            tool_calls=request.tool_calls,
        )
        result = component.process(run_input, phase=AgentPhase.RUNTIME)
        output = result.output
        reflection = output.reflection_result
        raw_pipeline = getattr(output, "raw_pipeline_result", None) or {}

        issues = [
            ReflectionIssue(
                issue_type=getattr(i, "issue_type", None),
                metric_name=getattr(i, "metric_name", None),
                explanation=getattr(i, "explanation", None),
                correction=getattr(i, "correction", None),
            )
            for i in reflection.issues
        ]

        decision = _decision_str(reflection.decision)
        score = _extract_overall_score(raw_pipeline)
        execution_ms = getattr(output, "execution_time_ms", None)
        log.info(
            "reflect session=%s track=%s decision=%s score=%s ms=%s",
            request.session_id or "-",
            track,
            decision,
            f"{score:.2f}" if score is not None else "-",
            f"{execution_ms:.1f}" if isinstance(execution_ms, (int, float)) else "-",
        )

        return ReflectResponse(
            decision=decision,
            issues=issues,
            overall_avg_score=score,
            execution_time_ms=execution_ms,
            raw_pipeline_result=raw_pipeline if self._settings.include_raw_response else None,
        )
