#!/usr/bin/env python3
"""
Validation Logic for Policy Builder

This module contains functions for validating generated policies, including
structural validation and semantic verification using LLM.
"""

import re
from typing import Any, Dict, List, Tuple

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import HumanMessage

from aiac_agent.agent.state import PolicyState


def validate_policy_structure(
    policy: Dict[str, Any],
    realm_roles: List[Dict[str, str]],
    client_names: List[str],
    client_roles_map: Dict[str, List[Dict[str, str]]],
) -> List[str]:
    """
    Perform structural validation on the policy.

    Checks that all realm roles, clients, and client roles exist in the
    configuration and that the policy structure is valid.

    Args:
        policy: The policy dictionary to validate
        realm_roles: List of dicts with 'name' and 'description' for realm roles
        client_names: List of valid client names
        client_roles_map: Dict mapping client names to list of role dicts with 'name' and 'description'

    Returns:
        List of error messages (empty if validation passed)
    """
    structural_errors = []

    if not policy:
        structural_errors.append("Policy is empty")
        return structural_errors

    # Extract realm role names for validation
    realm_role_names = [role["name"] for role in realm_roles]

    # Validate that only preset names are used
    for realm_role, client_role_mappings in policy.items():
        # Validate realm role name
        if not realm_role:
            structural_errors.append("Found empty realm role name")
        elif realm_role not in realm_role_names:
            structural_errors.append(
                f"Realm role '{realm_role}' is not in the preset realm roles. "
                f"Available roles: {', '.join(realm_role_names)}"
            )

        # Check if realm role has any mappings
        if not client_role_mappings:
            structural_errors.append(f"Realm role '{realm_role}' has no client role mappings assigned")

        # Validate each client role mapping
        for mapping in client_role_mappings:
            if not isinstance(mapping, dict):
                structural_errors.append(
                    f"Invalid mapping format in realm role '{realm_role}': must be a dict with 'client' and 'role' keys"
                )
                continue

            client = mapping.get("client", "")
            role = mapping.get("role", "")

            # Validate client name
            if not client:
                structural_errors.append(f"Found empty client name in realm role '{realm_role}'")
            elif client not in client_names:
                structural_errors.append(
                    f"Client '{client}' in realm role '{realm_role}' is not in "
                    f"the preset client names. Available clients: {', '.join(client_names)}"
                )

            # Validate role name for the client
            if not role:
                structural_errors.append(f"Found empty role name for client '{client}' in realm role '{realm_role}'")
            elif client in client_roles_map:
                # Extract role names from the client roles map
                client_role_names = [r["name"] for r in client_roles_map[client]]
                if role not in client_role_names:
                    available_roles = ", ".join(client_role_names) if client_role_names else "(none)"
                    structural_errors.append(
                        f"Role '{role}' for client '{client}' in realm role '{realm_role}' "
                        f"is not valid. Available roles for {client}: {available_roles}"
                    )

    return structural_errors


def verify_policy_semantics(
    state: PolicyState,
    llm: BaseChatModel,
    client_roles_map: Dict[str, List[Dict[str, str]]],
    client_audience_targets: Dict[str, List[str]],
    verbose: bool = True,
) -> Tuple[bool, bool, str]:
    """
    Use LLM to verify if the generated policy matches the original description.

    Args:
        state: Current PolicyState with description and policy_structure
        llm: LLM instance for verification
        client_roles_map: Dict mapping client names to list of role dicts with 'name' and 'description'
        client_audience_targets: Dict defining client call chains
        verbose: Whether to print verification details

    Returns:
        Tuple of (roles_correct, mappings_correct, explanation)
    """
    description = state.get("description", "")
    policy = state["policy_structure"].get("policy", {})

    # Format policy for verification with detailed role breakdown
    policy_summary = []
    for realm_role, client_role_mappings in policy.items():
        roles_by_client = {}
        for mapping in client_role_mappings:
            client = mapping.get("client", "")
            role = mapping.get("role", "")
            if client not in roles_by_client:
                roles_by_client[client] = []
            roles_by_client[client].append(role)

        policy_summary.append(f"  {realm_role}:")
        for client, roles in sorted(roles_by_client.items()):
            policy_summary.append(f"    - {client}: {', '.join(sorted(roles))}")

    policy_text = "\n".join(policy_summary)

    # Build available client roles context with descriptions
    client_roles_context = []
    for client, roles in client_roles_map.items():
        if roles:
            role_strs = []
            for role in roles:
                role_name = role["name"]
                role_desc = role.get("description", "")
                if role_desc:
                    role_strs.append(f"{role_name} ({role_desc})")
                else:
                    role_strs.append(role_name)
            client_roles_context.append(f"  - {client}: {', '.join(role_strs)}")
    client_roles_info = "\n".join(client_roles_context)

    # Build call chain context
    call_chain_info = ""
    if client_audience_targets:
        call_chain_lines = ["\n" + "=" * 80]
        call_chain_lines.append("CRITICAL - CLIENT CALL CHAINS (READ CAREFULLY):")
        call_chain_lines.append("=" * 80)
        call_chain_lines.append(
            "Some clients require access through a call chain. When a user needs access to a target client,"
        )
        call_chain_lines.append("they MUST also be granted roles on ALL intermediate clients in the chain.")
        call_chain_lines.append("")
        call_chain_lines.append("Call chain mappings:")
        for client, targets in client_audience_targets.items():
            if targets:
                call_chain_lines.append(f"  {client} → {', '.join(targets)}")
        call_chain_lines.append("")
        call_chain_lines.append("⚠️⚠️⚠️ CRITICAL VALIDATION RULES ⚠️⚠️⚠️")
        call_chain_lines.append("1. Call-chain roles are INFRASTRUCTURE REQUIREMENTS")
        call_chain_lines.append("2. These roles are ALWAYS REQUIRED when accessing downstream clients")
        call_chain_lines.append("3. DO NOT flag these as 'missing' - they are shown in the policy above")
        call_chain_lines.append("4. DO NOT flag these as 'extra' - they are required for the call chain to work")
        call_chain_lines.append("5. ONLY validate the FINAL TARGET roles against the description")
        call_chain_lines.append(
            "6. The description mentions access to resources, NOT the infrastructure needed to reach them"
        )
        call_chain_lines.append("=" * 80)
        call_chain_info = "\n".join(call_chain_lines)

    verification_prompt = f"""You are a policy validator. Verify that the generated policy matches the access \
requirements in the description.

ORIGINAL POLICY DESCRIPTION:
{description}

AVAILABLE CLIENT ROLES:
{client_roles_info}
{call_chain_info}

GENERATED POLICY MAPPING:
{policy_text}

VALIDATION RULES:

1. REALM ROLES (User Groups):
   - Use FLEXIBLE SEMANTIC MATCHING - consider synonyms, related terms, and context
   - "R&D", "developers", "technical staff", "technical personnel" are semantically equivalent
   - "support", "tech-support", "technical support", "technical personnel" can overlap semantically
   - "sales", "business", "other personnel", "other staff" are semantically equivalent
   - IMPORTANT: "technical personnel" and "tech-support" are semantically related - both refer to technical roles
   - Each user category in description should map to at least one realm role
   - CRITICAL: Broad/generic terms (like "other personnel", "other staff", "everyone else") typically map to \
MULTIPLE realm roles
   - Having MORE realm roles than user categories is CORRECT and EXPECTED when broad terms are used
   - Do NOT flag policies as incorrect for having more roles than categories - this is the correct behavior \
for broad terms
   - When only limited roles are available, accept reasonable semantic approximations

2. CLIENT ROLES (Permissions):
   - Only validate roles that exist in "AVAILABLE CLIENT ROLES" above
   - Call-chain/infrastructure roles (kagenti, spiffe) are ALWAYS required - ignore them in validation
   - Focus ONLY on the final target client roles (e.g., github-tool roles)
   - CRITICAL: Roles are COMPLETELY INDEPENDENT - one role does NOT include another
   - Role names are misleading - "full-access" does NOT automatically include other roles
   - ⚠️ CRITICAL FORMAT NOTE: In the policy display above, multiple roles for the same client are shown comma-separated
   - ⚠️ Example: "github-tool: github-full-access, github-tool-aud" means BOTH roles are assigned
   - ⚠️ This comma-separated format represents MULTIPLE SEPARATE role assignments - this is CORRECT
   - If description says "both X and Y", you should see BOTH roles listed (comma-separated)
   - If description says "only X", you should see ONLY the X role listed
   - Multiple roles for same client (shown comma-separated) is CORRECT when "both" is mentioned

ANSWER THESE TWO QUESTIONS:

Q1: Are the realm roles (user groups) correctly identified?
- Check if each user category from description has a corresponding realm role
- Use semantic matching, not literal text matching
- Answer: YES or NO

Q2: Are the client roles correctly mapped for the final target?
- Ignore call-chain roles (kagenti, spiffe) - they're infrastructure
- Check ONLY the final target roles (e.g., github-tool roles) against the description
- If description says "both X and Y", check that BOTH roles appear in the comma-separated list
- If description says "only X", check that ONLY X appears (not Y)
- Answer: YES or NO

⚠️⚠️⚠️ CRITICAL INSTRUCTION ⚠️⚠️⚠️
If you conclude in your explanation that "the mappings are actually correct" or "the policy matches the requirements",
then you MUST answer "MAPPINGS_CORRECT: YES". Your YES/NO answer MUST match your conclusion.
DO NOT say "NO" and then explain why it's correct - that's contradictory.

Respond in this EXACT format (do not deviate):
```
ROLES_CORRECT: YES
MAPPINGS_CORRECT: YES
EXPLANATION: Policy correctly maps all requirements.
```

OR if there are issues:
```
ROLES_CORRECT: NO
MAPPINGS_CORRECT: NO
EXPLANATION: Specific issue 1. Specific issue 2.
```"""

    try:
        messages = [HumanMessage(content=verification_prompt)]
        response = llm.invoke(messages)
        content = response.content if isinstance(response.content, str) else str(response.content)

        # Parse the structured response
        roles_match = re.search(r"ROLES_CORRECT:\s*(YES|NO)", content, re.IGNORECASE)
        mappings_match = re.search(r"MAPPINGS_CORRECT:\s*(YES|NO)", content, re.IGNORECASE)
        explanation_match = re.search(r"EXPLANATION:\s*(.+?)(?:\n```|$)", content, re.DOTALL | re.IGNORECASE)

        roles_correct = roles_match and roles_match.group(1).upper() == "YES" if roles_match else False
        mappings_correct = mappings_match and mappings_match.group(1).upper() == "YES" if mappings_match else False
        explanation = explanation_match.group(1).strip() if explanation_match else content

        # Print verification details if verbose or if there are errors
        if verbose or not roles_correct or not mappings_correct:
            print("\n" + "=" * 80)
            print("POLICY VERIFICATION:")
            print("=" * 80)
            print(f"Roles Correct: {'YES' if roles_correct else 'NO'}")
            print(f"Mappings Correct: {'YES' if mappings_correct else 'NO'}")
            print(f"Explanation: {explanation}")
            print("=" * 80 + "\n")

        return (roles_correct, mappings_correct, explanation)

    except Exception as e:
        error_msg = str(e)
        # Handle "Already borrowed" errors gracefully
        if "Already borrowed" in error_msg or "BadRequestError" in error_msg:
            return (False, False, "Verification skipped due to transient API error; retry required")
        return (False, False, f"Could not verify policy: {error_msg}")


# Made with Bob
