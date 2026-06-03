"""Wire models for the SPARC reflection service.

The request mirrors SPARC's native input — an OpenAI-style conversation, the
available tool inventory, and the proposed tool call(s). The response is a thin,
faithful projection of SPARC's verdict; enforcement policy is the caller's job.
"""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel, Field


class ReflectRequest(BaseModel):
    """Inputs SPARC needs to judge a proposed tool call."""

    messages: list[dict[str, Any]] = Field(
        default_factory=list,
        description="OpenAI-style conversation context (role/content turns).",
    )
    tool_specs: list[dict[str, Any]] = Field(
        default_factory=list,
        description="Available tools in OpenAI function-calling format.",
    )
    tool_calls: list[dict[str, Any]] = Field(
        ...,
        min_length=1,
        description="Proposed tool call(s); SPARC evaluates the first.",
    )
    session_id: str | None = Field(default=None, description="Opaque correlation id for logs/observability.")
    track: str | None = Field(
        default=None,
        description="Per-request SPARC track override; falls back to the service's SPARC_TRACK.",
    )


class ReflectionIssue(BaseModel):
    """A single grounding/appropriateness problem SPARC found."""

    issue_type: str | None = None
    metric_name: str | None = None
    explanation: str | None = None
    correction: Any | None = None


class ReflectResponse(BaseModel):
    """SPARC's verdict, faithfully projected for the AuthBridge plugin."""

    decision: str = Field(description="approve | reject | error")
    issues: list[ReflectionIssue] = Field(default_factory=list)
    overall_avg_score: float | None = Field(
        default=None,
        description="SPARC's grounding score, normalized 0..1 (higher = better grounded), when available.",
    )
    execution_time_ms: float | None = None
    raw_pipeline_result: dict[str, Any] | None = Field(
        default=None,
        description="Full SPARC pipeline detail; included only when SPARC_INCLUDE_RAW_RESPONSE is set.",
    )
