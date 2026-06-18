#!/usr/bin/env python3
"""
Parsing Utilities for Policy Builder

This module contains functions for parsing LLM responses, extracting JSON
and explanations from formatted output.
"""

import json
import re
from typing import List, Tuple


def extract_explanation_and_json(content: str) -> Tuple[str, List]:
    """
    Extract explanation and JSON from formatted LLM output.

    This function parses the LLM's response to extract two components:
    1. The explanation text (from ```explanation``` block or pre-JSON text)
    2. The JSON data (from ```json``` block or plain JSON)

    It handles multiple formats:
    - Markdown code blocks with ```explanation``` and ```json``` tags
    - Plain text explanation followed by ```json``` block
    - Plain text with "explanation" header followed by JSON array
    - Plain JSON without code blocks (fallback)

    Args:
        content: Raw LLM response content string

    Returns:
        Tuple of (explanation_text, parsed_json_list)
        Returns empty string and empty list if parsing fails

    Example:
        >>> content = '''```explanation
        ... Mapping admins to all roles
        ... ```
        ... ```json
        ... [{"role": "admin", "client_roles": [...]}]
        ... ```'''
        >>> explanation, data = extract_explanation_and_json(content)
    """
    explanation = ""

    # Try to extract explanation from dedicated code block
    explanation_match = re.search(r"```explanation\s*([\s\S]*?)\s*```", content)
    if explanation_match:
        explanation = explanation_match.group(1).strip()
    else:
        # Fallback 1: Try to extract explanation from text before JSON block
        # Look for text before the first JSON code block
        json_start = re.search(r"```json", content)
        if json_start:
            pre_json_text = content[: json_start.start()].strip()
            # Remove markdown formatting (bold, italic, etc.)
            pre_json_text = re.sub(r"\*\*([^*]+)\*\*", r"\1", pre_json_text)
            # Only use if substantial (more than 10 characters)
            if pre_json_text and len(pre_json_text) > 10:
                explanation = pre_json_text
        else:
            # Fallback 2: Try to extract explanation from plain text with "explanation" header
            # Pattern: "explanation\n- text\n- more text\n[{...}]"
            explanation_header_match = re.search(r"^explanation\s*\n([\s\S]*?)(?=\[\s*\{)", content, re.IGNORECASE)
            if explanation_header_match:
                explanation = explanation_header_match.group(1).strip()

    # Try to extract JSON from markdown code blocks
    code_block_patterns = [
        r"```json\s*([\s\S]*?)\s*```",  # ```json ... ```
        r"```\s*([\s\S]*?)\s*```",  # ``` ... ``` (generic)
    ]

    for pattern in code_block_patterns:
        match = re.search(pattern, content)
        if match:
            try:
                return explanation, json.loads(match.group(1).strip())
            except json.JSONDecodeError:
                # Try next pattern if this one fails
                continue

    # Fallback: Try to find JSON array in plain text (after explanation header if present)
    # Look for JSON array pattern: [ ... ]
    json_array_match = re.search(r"\[\s*\{[\s\S]*\}\s*\]", content)
    if json_array_match:
        try:
            return explanation, json.loads(json_array_match.group(0))
        except json.JSONDecodeError:
            pass

    # Final fallback: Try plain JSON without code blocks
    try:
        return explanation, json.loads(content.strip())
    except json.JSONDecodeError:
        # Return empty list if all parsing attempts fail
        return explanation, []


def print_explanation(explanation: str, is_retry: bool = False, verbose: bool = True):
    """
    Print the LLM explanation in a formatted box.

    This function provides visual feedback to the user about how the LLM
    interpreted and mapped the policy description. Only prints if verbose
    mode is enabled.

    Args:
        explanation: The explanation text to print
        is_retry: Whether this is from a retry attempt (adds "(Retry)" label)
        verbose: Whether to print the explanation
    """
    if explanation and verbose:
        print("\n" + "=" * 80)
        print(f"LLM Explanation{' (Retry)' if is_retry else ''}:")
        print("=" * 80)
        print(explanation)
        print("=" * 80 + "\n")


# Made with Bob
