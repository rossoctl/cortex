#!/usr/bin/env python3
"""
Policy Builder - Main Module

This module provides the main PolicyBuilder class that orchestrates the
AI-powered generation of Keycloak access control policies from natural
language descriptions using LangGraph workflows.

Refactored to follow official LangGraph patterns:
- Separation of graph definition from business logic
- Pure node functions for better testability
- Proper type hints and annotations
- Configuration as a separate concern
- Support for graph visualization

The PolicyBuilder has been refactored into multiple modules for better
organization and maintainability:
- state.py: State definitions
- config_utils.py: Configuration loading and parsing
- constants.py: Constants
- prompt_builder.py: LLM prompt construction
- parsers.py: Response parsing utilities
- validators.py: Policy validation logic
- cli.py: Command-line interface

Key Features:
    - Natural language to YAML policy conversion
    - Automatic role mapping and validation
    - Call chain analysis and enforcement
    - Retry mechanism with semantic verification
"""

import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Optional

import yaml
from langchain_core.language_models import BaseChatModel
from langchain_core.messages import HumanMessage, SystemMessage
from langgraph.graph import END, StateGraph

from aiac_agent.agent.state import PolicyState
from aiac_agent.config import create_llm
from aiac_agent.config.config_utils import extract_realm_roles_and_clients, load_config
from aiac_agent.config.constants import MAX_VALIDATION_RETRIES
from aiac_agent.prompts.prompt_builder import build_retry_prompt, build_system_prompt
from aiac_agent.utils.parsers import extract_explanation_and_json, print_explanation
from aiac_agent.utils.validators import validate_policy_structure, verify_policy_semantics


@dataclass
class PolicyBuilderConfig:
    """
    Configuration for PolicyBuilder agent.

    Following LangGraph best practices, configuration is separated from
    the agent logic for better testability and flexibility.

    Attributes:
        config_path: Path to configuration file
        llm: LangChain LLM instance
        verbose: Whether to print detailed output
        max_retries: Maximum validation retry attempts
    """

    config_path: Path
    llm: BaseChatModel
    verbose: bool = True
    max_retries: int = MAX_VALIDATION_RETRIES


# ============================================================================
# PURE NODE FUNCTIONS (Following LangGraph Best Practices)
# ============================================================================
# These functions are pure and stateless, making them easier to test and reason about


def _parse_and_extract_scopes(
    state: PolicyState,
    llm: BaseChatModel,
    realm_roles: list,
    client_roles_map: dict,
    client_audience_targets: dict,
    verbose: bool,
) -> PolicyState:
    """
    Parse natural language description and extract role-to-client-role mappings.

    This is the first node in the workflow. It sends the policy description
    to the LLM and extracts structured JSON mappings with automatic retry.

    Args:
        state: Current PolicyState with 'description' field
        llm: LLM instance for processing
        realm_roles: List of available realm roles
        client_roles_map: Dict mapping client names to roles
        client_audience_targets: Dict defining call chains
        verbose: Whether to print detailed output

    Returns:
        Updated PolicyState with parsed_scopes and explanation

    Raises:
        ValueError: If JSON parsing fails after retry attempt
    """
    # Build prompts
    system_prompt = build_system_prompt(realm_roles, client_roles_map, client_audience_targets)
    user_prompt = (
        f"Parse this policy description and map it to the preset role and scope names:\n\n{state['description']}"
    )

    # First attempt
    messages = [SystemMessage(content=system_prompt), HumanMessage(content=user_prompt)]

    response = llm.invoke(messages)
    content = response.content if isinstance(response.content, str) else str(response.content)
    explanation, parsed_scopes = extract_explanation_and_json(content)

    # Print explanation if available
    print_explanation(explanation, verbose=verbose)

    # Retry once if parsing failed
    if not parsed_scopes:
        retry_prompt = build_retry_prompt(realm_roles, client_roles_map)

        retry_messages = [*messages, response, HumanMessage(content=retry_prompt)]

        retry_response = llm.invoke(retry_messages)
        retry_content = (
            retry_response.content if isinstance(retry_response.content, str) else str(retry_response.content)
        )
        explanation, parsed_scopes = extract_explanation_and_json(retry_content)

        # Print retry explanation
        print_explanation(explanation, is_retry=True, verbose=verbose)

        # If still failed after retry, raise exception
        if not parsed_scopes:
            raise ValueError(
                f"Failed to parse valid JSON from LLM response after retry.\nLast response: {retry_content[:500]}..."
            )

    # Return updated state
    return {
        **state,
        "explanation": explanation,
        "parsed_scopes": parsed_scopes,
        "messages": [*state.get("messages", []), response],
        "errors": [],  # Clear errors on new parse attempt
        "retry_count": state.get("retry_count", 0),
        "validation_passed": True,  # Assume passed until validation runs
    }


def _build_policy(state: PolicyState) -> PolicyState:
    """
    Build structured policy dictionary from extracted role mappings.

    This is the second node in the workflow.

    Args:
        state: PolicyState with 'parsed_scopes' field

    Returns:
        Updated PolicyState with 'policy_structure' field
    """
    policy = {}

    # Transform parsed scopes into policy structure
    for role_info in state["parsed_scopes"]:
        role_name = role_info.get("role", "")
        client_roles = role_info.get("client_roles", [])
        policy[role_name] = client_roles

    # Wrap in policy structure
    policy_structure = {"policy": policy}

    return {**state, "policy_structure": policy_structure}


def _generate_yaml(state: PolicyState) -> PolicyState:
    """
    Generate YAML output from the policy structure with comprehensive comments.

    This is the third node in the workflow.

    Args:
        state: PolicyState with 'policy_structure', 'description', and 'explanation'

    Returns:
        Updated PolicyState with 'yaml_output' field
    """
    # Create header comments
    header = """# Access Control Policy
# Maps user roles (realm roles) to specific client roles
# Format: user_role_name -> list of client role mappings
# Each entry specifies: client (client name) and role (role name from that client)

"""

    # Add original policy description as comment
    if state.get("description"):
        description_lines = state["description"].strip().split("\n")
        header += "# Original Policy Description:\n"
        for line in description_lines:
            header += f"#   {line.strip()}\n"
        header += "#\n"

    # Add LLM explanation as comment
    if state.get("explanation"):
        explanation_lines = state["explanation"].strip().split("\n")
        header += "# LLM Mapping Explanation:\n"
        for line in explanation_lines:
            header += f"#   {line.strip()}\n"
        header += "\n"

    # Generate YAML from policy structure
    yaml_content = yaml.dump(state["policy_structure"], default_flow_style=False, sort_keys=False, allow_unicode=True)

    # Add footer
    footer = "\n# Generated by PolicyBuilder using LangGraph\n"

    # Combine all parts
    yaml_output = header + yaml_content + footer

    return {**state, "yaml_output": yaml_output}


def _validate_policy(
    state: PolicyState,
    llm: BaseChatModel,
    realm_roles: list,
    client_names: list,
    client_roles_map: dict,
    client_audience_targets: dict,
    verbose: bool,
    max_retries: int,
) -> PolicyState:
    """
    Validate the generated policy structure and verify it matches the description.

    This is the fourth and final node in the workflow. It performs structural
    validation and semantic verification.

    Args:
        state: PolicyState with 'policy_structure' and 'description'
        llm: LLM instance for semantic verification
        realm_roles: List of available realm roles
        client_names: List of client names
        client_roles_map: Dict mapping client names to roles
        client_audience_targets: Dict defining call chains
        verbose: Whether to print detailed output
        max_retries: Maximum retry attempts

    Returns:
        Updated PolicyState with errors and validation_passed fields
    """
    retry_count = state.get("retry_count", 0)
    policy = state["policy_structure"].get("policy", {})

    # Perform structural validation
    structural_errors = validate_policy_structure(policy, realm_roles, client_names, client_roles_map)

    # If there are structural errors and we can retry, trigger retry
    if structural_errors and retry_count < max_retries:
        return {**state, "errors": structural_errors, "validation_passed": False, "retry_count": retry_count + 1}

    # Perform semantic validation if structural validation passed
    verification_errors = []
    if not structural_errors:
        roles_correct, mappings_correct, explanation = verify_policy_semantics(
            state, llm, client_roles_map, client_audience_targets, verbose
        )

        # Add verification errors if checks failed
        if not roles_correct:
            verification_errors.append(f"User roles incorrectly identified: {explanation}")
        if not mappings_correct:
            verification_errors.append(f"Client roles incorrectly mapped: {explanation}")

    # Determine if validation passed
    validation_passed = len(structural_errors) == 0 and len(verification_errors) == 0

    # If there are verification errors and we can retry, trigger retry
    if verification_errors and retry_count < max_retries:
        return {**state, "errors": verification_errors, "validation_passed": False, "retry_count": retry_count + 1}

    # Return final result with all errors
    all_errors = structural_errors + verification_errors
    return {**state, "errors": all_errors, "validation_passed": validation_passed, "retry_count": retry_count}


def _should_retry_validation(state: PolicyState, max_retries: int) -> str:
    """
    Determine if validation should retry by going back to parse_and_extract.

    This is a conditional edge function for the LangGraph state machine.

    Args:
        state: Current PolicyState containing validation results
        max_retries: Maximum retry attempts allowed

    Returns:
        "parse_and_extract" if validation failed and retries remain,
        otherwise END to terminate the workflow
    """
    validation_passed = state.get("validation_passed", False)
    retry_count = state.get("retry_count", 0)
    errors = state.get("errors", [])

    # If validation failed and we haven't exceeded max retries, retry from start
    if not validation_passed and retry_count < max_retries:
        print(f"\n⚠️  Validation failed (attempt {retry_count}/{max_retries}). Retrying from parse_and_extract...")
        if errors:
            print("\nValidation Errors (from this attempt):")
            for i, error in enumerate(errors, 1):
                print(f"  {i}. {error}")
            print()
        return "parse_and_extract"

    # Either validation passed or max retries exceeded
    return END


def create_policy_builder_graph(
    config: PolicyBuilderConfig,
    realm_roles: list,
    client_roles_map: dict,
    client_audience_targets: dict,
    client_names: list,
):
    """
    Create and compile the policy builder graph.

    Following LangGraph patterns, this function separates graph construction
    from the agent class, making it easier to test and visualize.

    Args:
        config: PolicyBuilderConfig instance
        realm_roles: List of available realm roles
        client_roles_map: Dict mapping client names to roles
        client_audience_targets: Dict defining call chains
        client_names: List of client names

    Returns:
        Compiled LangGraph workflow
    """

    # Define node functions as closures with access to config
    def parse_and_extract_node(state: PolicyState) -> PolicyState:
        """Parse natural language and extract role mappings."""
        return _parse_and_extract_scopes(
            state, config.llm, realm_roles, client_roles_map, client_audience_targets, config.verbose
        )

    def build_policy_node(state: PolicyState) -> PolicyState:
        """Build structured policy from mappings."""
        return _build_policy(state)

    def generate_yaml_node(state: PolicyState) -> PolicyState:
        """Generate YAML output with comments."""
        return _generate_yaml(state)

    def validate_policy_node(state: PolicyState) -> PolicyState:
        """Validate structure and semantics."""
        return _validate_policy(
            state,
            config.llm,
            realm_roles,
            client_names,
            client_roles_map,
            client_audience_targets,
            config.verbose,
            config.max_retries,
        )

    def should_retry_node(state: PolicyState) -> str:
        """Determine if validation should retry."""
        return _should_retry_validation(state, config.max_retries)

    # Build the graph
    workflow = StateGraph(PolicyState)

    # Add nodes
    workflow.add_node("parse_and_extract", parse_and_extract_node)
    workflow.add_node("build_policy", build_policy_node)
    workflow.add_node("generate_yaml", generate_yaml_node)
    workflow.add_node("validate_policy", validate_policy_node)

    # Define edges
    workflow.set_entry_point("parse_and_extract")
    workflow.add_edge("parse_and_extract", "build_policy")
    workflow.add_edge("build_policy", "generate_yaml")
    workflow.add_edge("generate_yaml", "validate_policy")

    # Add conditional edge for retry logic
    workflow.add_conditional_edges(
        "validate_policy", should_retry_node, {"parse_and_extract": "parse_and_extract", END: END}
    )

    return workflow.compile()


class PolicyBuilder:
    """
    AI-powered access control policy builder using LangGraph.

    Refactored to follow official LangGraph patterns:
    - Configuration separated from logic
    - Graph construction delegated to factory function
    - Pure node functions for better testability
    - Support for graph visualization

    This class orchestrates a multi-stage workflow to convert natural language
    policy descriptions into structured YAML access control policies.

    Workflow Stages:
        1. parse_and_extract: Parse natural language and extract role mappings
        2. build_policy: Build structured policy from mappings
        3. generate_yaml: Generate YAML output with comments
        4. validate_policy: Validate structure and semantics (with retry)

    Attributes:
        config: PolicyBuilderConfig instance
        realm_roles: List of available realm role names
        client_roles_map: Dict mapping client names to their available roles
        client_audience_targets: Dict defining client call chains
        client_names: List of client names
        graph: Compiled LangGraph state machine
    """

    def __init__(
        self,
        config_path: Path,
        llm: Optional[BaseChatModel] = None,
        verbose: bool = True,
        max_retries: int = MAX_VALIDATION_RETRIES,
    ):
        """
        Initialize the policy builder with configuration and LLM.

        Args:
            config_path: Path to config file containing realm roles, clients,
                        and call chain definitions (required)
            llm: Optional LangChain LLM instance. If not provided, creates a new
                 LLM instance using create_llm()
            verbose: If True, print LLM explanations and validation details
            max_retries: Maximum validation retry attempts

        Raises:
            FileNotFoundError: If config_path doesn't exist
            yaml.YAMLError: If config file is invalid YAML
        """
        # Create LLM if not provided
        # LLM config is in the config directory relative to this file (llm.env)
        if llm is None:
            llm_env_path = Path(__file__).parent.parent / "config" / "llm.env"
            llm_instance = create_llm(env_path=llm_env_path, verbose=verbose)
        else:
            llm_instance = llm

        # Create configuration object
        self.config = PolicyBuilderConfig(
            config_path=config_path, llm=llm_instance, verbose=verbose, max_retries=max_retries
        )

        # Load config and extract realm roles, client roles map, and call chains
        config_data = load_config(config_path)
        self.realm_roles, self.client_roles_map, self.client_audience_targets = extract_realm_roles_and_clients(
            config_data
        )

        # Build flat list of client names
        self.client_names = list(self.client_roles_map.keys())

        # Build and compile the LangGraph state machine
        self.graph = create_policy_builder_graph(
            self.config, self.realm_roles, self.client_roles_map, self.client_audience_targets, self.client_names
        )

    # ========================================================================
    # GRAPH VISUALIZATION AND INSPECTION
    # ========================================================================

    def get_graph(self):
        """
        Get the compiled graph for visualization or inspection.

        Following LangGraph patterns, this allows external tools to
        visualize or analyze the graph structure.

        Returns:
            Compiled LangGraph workflow
        """
        return self.graph

    # ========================================================================
    # PUBLIC API METHODS
    # ========================================================================

    def generate_policy(self, description: str) -> Dict[str, Any]:
        """
        Generate an access control policy from a natural language description.

        This is the main public API method. It executes the complete workflow.

        Args:
            description: Natural language description of the access control policy

        Returns:
            Dictionary containing:
                - yaml_output (str): Complete YAML policy file content
                - policy_structure (dict): Structured policy data
                - parsed_scopes (list): Raw role-to-client-role mappings from LLM
                - errors (list): Validation errors (empty if successful)
                - success (bool): True if generation succeeded without errors
                - retry_count (int): Number of validation retries that occurred

        Example:
            >>> builder = PolicyBuilder(config_path=Path("config.yaml"))
            >>> result = builder.generate_policy("Admins have full access")
            >>> if result["success"]:
            ...     print(result["yaml_output"])
        """
        # Initialize the workflow state
        initial_state: PolicyState = {
            "description": description,
            "explanation": "",
            "parsed_scopes": [],
            "policy_structure": {},
            "yaml_output": "",
            "messages": [],
            "errors": [],
            "retry_count": 0,
            "validation_passed": True,
        }

        # Execute the LangGraph workflow
        final_state = self.graph.invoke(initial_state)

        # Extract and return results
        return {
            "yaml_output": final_state["yaml_output"],
            "policy_structure": final_state["policy_structure"],
            "parsed_scopes": final_state["parsed_scopes"],
            "errors": final_state["errors"],
            "success": len(final_state["errors"]) == 0,
            "retry_count": final_state.get("retry_count", 0),
        }

    def save_policy(self, yaml_output: str, filepath: str = "access_control_policy.yaml"):
        """
        Save the generated policy YAML to a file.

        Args:
            yaml_output: YAML content string to save
            filepath: Output file path (default: "access_control_policy.yaml")
        """
        with open(filepath, "w") as f:
            f.write(yaml_output)
        print(f"Access rules saved to {filepath}")


# ============================================================================
# BACKWARD COMPATIBILITY
# ============================================================================
# For backward compatibility, keep the main() function here but delegate to CLI
if __name__ == "__main__":
    # This file should not be run directly anymore
    # Use main.py in the parent directory instead
    print("Please use main.py to run the policy builder:")
    print("  python main.py <policy_file.txt> <config.yaml> <output_file.yaml>")
    sys.exit(1)

# Made with Bob
