#!/usr/bin/env python3
"""
Constants for Policy Builder

This module defines constants used throughout the policy builder system.
"""

# Maximum number of retry cycles from validate_policy back to parse_and_extract
# This prevents infinite loops while allowing the LLM to self-correct
MAX_VALIDATION_RETRIES = 3

# Made with Bob
