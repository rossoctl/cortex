"""Rossoctl SPARC reflection service.

A thin, in-process HTTP wrapper around the SPARC pre-tool reflection component
(`altk.pre_tool.sparc.SPARCReflectionComponent`) shipped in the
``agent-lifecycle-toolkit`` PyPI package.

The service exposes a single reflection endpoint that AuthBridge's Go ``sparc``
plugin calls to decide whether a proposed tool call is grounded in the
conversation and tool specifications. All enforcement policy (observe / inject /
deny, score thresholds) lives in the plugin; this service only returns SPARC's
verdict faithfully.
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
