#!/usr/bin/env python3
"""
State Definitions for Policy Builder

This module defines the TypedDict state structure used by the LangGraph
workflow for policy generation.
"""

from operator import add
from typing import Annotated, Any, Dict, List, TypedDict


class PolicyState(TypedDict):
    """
    State dictionary for the policy building LangGraph workflow.

    This TypedDict defines the state that flows through the state machine.
    Each node in the graph can read from and write to these fields.

    Attributes:
        description: Original natural language policy description
        explanation: LLM's explanation of how it mapped the policy
        parsed_scopes: List of role-to-client-role mappings extracted by LLM
        policy_structure: Structured policy dictionary ready for YAML conversion
            Format: {"policy": {"realm_role": [{"client": "...", "role": "..."}]}}
        yaml_output: Final YAML-formatted policy string with comments and documentation
        messages: Accumulated list of LLM messages for conversation history
            Annotated with 'add' operator for automatic accumulation across nodes
        errors: List of validation error messages (replaced on each validation attempt)
            NOT accumulated - each validation replaces the previous error list
        retry_count: Number of validation retry attempts made (0 = first attempt)
        validation_passed: Boolean flag indicating if validation succeeded
    """

    description: str
    explanation: str
    parsed_scopes: List[Dict[str, Any]]
    policy_structure: Dict[str, Any]
    yaml_output: str
    messages: Annotated[List, add]  # Annotated with 'add' for accumulation
    errors: List[str]  # NOT accumulated - replaced on each validation attempt
    retry_count: int
    validation_passed: bool  # Boolean flag for retry decision, not accumulated
