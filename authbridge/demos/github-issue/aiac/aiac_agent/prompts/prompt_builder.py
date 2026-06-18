#!/usr/bin/env python3
"""
Prompt Builder for Policy Generation

This module contains functions for building LLM prompts used in policy generation,
including system prompts and retry prompts.
"""

from typing import Dict, List


def build_system_prompt(
    realm_roles: List[Dict[str, str]],
    client_roles_map: Dict[str, List[Dict[str, str]]],
    client_audience_targets: Dict[str, List[str]],
) -> str:
    """
    Build the comprehensive system prompt for the LLM.

    This function constructs a detailed system prompt that includes:
    - Available realm roles from configuration with descriptions
    - Client roles for each client with descriptions
    - Call chain relationships
    - Critical rules for role assignment
    - Task instructions and output format

    The prompt is carefully designed to guide the LLM to:
    1. Use only predefined role names (no modifications)
    2. Assign all mentioned access types as separate entries
    3. Include complete call chains when needed
    4. Distinguish between "both X and Y" vs "only X" patterns
    5. Leverage role descriptions for better semantic understanding

    Args:
        realm_roles: List of dicts with 'name' and 'description' for realm roles
        client_roles_map: Dict mapping client names to list of role dicts with 'name' and 'description'
        client_audience_targets: Dict defining client call chains

    Returns:
        Formatted system prompt string ready for LLM consumption
    """
    # Build available realm roles list with formatting and descriptions
    available_roles_lines = []
    for role in realm_roles:
        role_name = role["name"]
        role_desc = role.get("description", "")
        if role_desc:
            available_roles_lines.append(f"  - {role_name}: {role_desc}")
        else:
            available_roles_lines.append(f"  - {role_name}")

    available_roles = "\n".join(available_roles_lines) if available_roles_lines else "  (none defined)"

    # Build client roles information showing which roles each client has with descriptions
    client_roles_info = []
    for client_name, roles in client_roles_map.items():
        if roles:
            role_strs = []
            for role in roles:
                role_name = role["name"]
                role_desc = role.get("description", "")
                if role_desc:
                    role_strs.append(f"{role_name} ({role_desc})")
                else:
                    role_strs.append(role_name)
            client_roles_info.append(f"  - {client_name}:\n      {', '.join(role_strs)}")

    client_roles_str = "\n".join(client_roles_info) if client_roles_info else "  (no client roles defined)"

    # Build call chain information showing client-to-client relationships
    call_chains = []
    for client, targets in client_audience_targets.items():
        if targets:
            call_chains.append(f"  - {client} can call: {', '.join(targets)}")
    call_chain_info = "\n".join(call_chains) if call_chains else "  (no call chains defined)"

    return f"""You are an expert at mapping access control policy descriptions to predefined user roles and \
application clients as defined in Keycloak.

ROLE DESCRIPTIONS GUIDANCE:
The roles below include descriptions that explain their purpose and permissions. Use these descriptions to:
1. Better understand what each role represents semantically
2. Match natural language policy descriptions to the appropriate roles
3. Determine which client roles align with the access requirements described in the policy
4. Make more accurate mappings by considering both role names AND their descriptions

⚠️⚠️⚠️ CRITICAL RULE #1 - READ THIS FIRST - THIS IS THE MOST IMPORTANT RULE ⚠️⚠️⚠️
    When a policy says "both X and Y" or mentions multiple access types, you MUST assign ALL corresponding \
roles as SEPARATE entries.
    - "both X and Y" = You MUST create TWO separate role entries: one for X AND one for Y
    - Count the access types mentioned: if 2 types are mentioned, you MUST have 2 separate role entries
    - DO NOT consolidate. DO NOT assume one role includes another. DO NOT skip any roles.
    - ALWAYS assign ALL mentioned access types as separate role entries.
    - If you see "both", "and", or multiple access types, STOP and count them, then create that many separate entries.
    - CRITICAL: When you see "both", you MUST generate EXACTLY 2 separate client role entries for that client
    - DO NOT generate only 1 role when "both" is mentioned - this is WRONG and will cause access control failures
    ⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️

CRITICAL REQUIREMENTS:
1. Use ONLY the preset names listed below - no modifications, no new names
2. Map natural language descriptions to the appropriate preset role, client, and client role names
3. Each realm role should specify which client roles from which clients users with that role need
4. Consider the client call chains when determining which clients a role needs access to
5. IMPORTANT: If a user needs access to a tool, they MUST get access to the COMPLETE call chain from the entry point
   - Example: If the call chain is UI → Agent → Tool, and a user needs the Tool, they need access to UI, Agent, AND Tool
   - Users need access to EVERY client in the complete path, starting from the first entry point (typically a UI client)
   - ALWAYS include the entry point client (e.g., demo-ui) when users need access to any downstream tool or agent
6. ⚠️ CRITICAL: Assign ALL client roles whose descriptions match the access requirements
   - Roles are INDEPENDENT and ADDITIVE - they do NOT overlap or include each other
   - If a requirement mentions multiple access types (e.g., "both X and Y"), assign ALL roles that provide \
those access types
   - DO NOT assume one role includes another - treat each role as providing distinct, non-overlapping permissions

Available realm roles (user roles - use ONLY these exact names):
{available_roles}

Available clients with their roles:
{client_roles_str}

Client call chains (which clients can call which other clients):
{call_chain_info}

⚠️⚠️⚠️ CRITICAL RULE #2 - CALL CHAIN REQUIREMENTS - NEVER SKIP INTERMEDIATE CLIENTS ⚠️⚠️⚠️
MANDATORY CALL CHAIN ALGORITHM - FOLLOW THESE EXACT STEPS:
1. Identify the FINAL TARGET client the user needs (e.g., the tool they want to access)
2. Look up that client in the "Client call chains" section above
3. Trace BACKWARDS through the chain to find ALL intermediate clients
4. Count the total number of clients in the complete path (entry point → intermediates → target)
5. Create a client_roles entry for EVERY SINGLE client in that path - NO EXCEPTIONS
6. Verify your count: if the chain has 3 clients, you MUST have 3 client_roles entries

CRITICAL RULES:
- EVERY client in the chain path MUST have a corresponding entry in client_roles
- Intermediate clients are NOT optional - they are MANDATORY infrastructure
- If you skip even ONE intermediate client, the entire call chain will fail
- DO NOT assume intermediate clients are implied - you MUST explicitly list them
- Count your entries and match them to the chain length - they MUST be equal
⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️⚠️

IMPORTANT ROLE ASSIGNMENT RULES - MEMORIZE THESE:
1. COMPREHENSIVE ACCESS ("both", "all", "full", "multiple types", "and"):
   - "both X and Y" → assign BOTH the X role AND the Y role as separate entries
   - "all types" → assign ALL available roles for that client
   - NEVER consolidate - each access type gets its own role entry

2. LIMITED ACCESS ("only", "just", "exclusively", "X only"):
   - "only X" → assign ONLY the X role, NOT Y
   - The word "only" means EXCLUSIVE - do not add other roles
   - ⚠️ CRITICAL: If one group gets "both X and Y" but another gets "only Y", they get DIFFERENT roles

3. ROLE INDEPENDENCE:
   - Each client role grants SPECIFIC, INDEPENDENT permissions
   - Roles are NOT hierarchical - do not assume one includes another
   - ALWAYS assign each mentioned access type explicitly

4. MULTIPLE ROLES FOR SAME CLIENT:
   - If a client has multiple roles and policy requires multiple access types, create SEPARATE entries for EACH role
   - DO NOT consolidate into one entry - list them separately

5. BROAD CATEGORY TERMS - CRITICAL FOR COMPREHENSIVE COVERAGE:
   - ⚠️ "other personnel", "other staff", "other users", "everyone else" → map to ALL realm roles not already mapped
   - ⚠️ CRITICAL: Broad terms typically map to MULTIPLE realm roles (e.g., "other personnel" → both "sales" \
AND "tech-support")
   - Be INCLUSIVE rather than exclusive - if a realm role isn't explicitly mentioned elsewhere, include it \
under the broad term
   - ⚠️ BUT: Still respect access restrictions - if "other personnel" gets "only X", don't give them Y

6. DIFFERENT ACCESS LEVELS FOR DIFFERENT GROUPS:
   - ⚠️ CRITICAL: Each user group may have DIFFERENT access levels to the same client
   - Read the description carefully for EACH group separately
   - Do NOT copy roles from one group to another - analyze each group independently

TASK STEPS - FOLLOW IN ORDER:
1. Read the policy description carefully - HIGHLIGHT any occurrence of "both", "and", "all" keywords
2. Identify each user group → map to realm role (use semantic matching)
   - ⚠️ CRITICAL: Broad terms like "other personnel" typically map to MULTIPLE realm roles
   - Count available realm roles, subtract explicitly mentioned ones, assign remaining to broad term
3. For EACH user group SEPARATELY, list their specific access requirements:
   - What does THIS group need? (not what other groups need)
   - Does THIS group get "both X and Y" or "only Y"?
   - Write down the exact roles THIS group should receive
   - ⚠️ If you see "both", write down BOTH roles explicitly - do not skip any
4. ⚠️ CRITICAL: Different groups get DIFFERENT roles - do NOT copy roles between groups
5. Count access types for EACH group: "both X and Y" = 2 roles, "only Y" = 1 role
6. Map each access type to its client role from the available roles list
7. For each target client, trace the complete call chain and include ALL clients in the path
8. ⚠️ CRITICAL PRE-GENERATION CHECK - Before generating JSON, for EACH realm role:
   - ✓ Does this group's description say "both"? → You MUST create 2 separate client_roles entries for that client
   - ✓ Does this group's description say "only"? → Create only 1 client_roles entry for that client
   - ✓ Did I analyze this group independently (not copy from another group)?
   - ✓ All call chain clients are included
   - ✓ Count: if "both X and Y" → must have 2 entries with same client but different roles
   - ✓ VERIFY: For each "both" in description, I have created EXACTLY 2 separate role entries
9. Write brief explanation of your mapping decisions (mention when you assigned multiple roles for same client)
10. Generate JSON - create separate {{"client": "...", "role": "..."}} entry for EACH role
   - ⚠️ REMINDER: If description said "both", you MUST list 2 separate entries with same client but different roles
11. ⚠️ FINAL VERIFICATION: Re-count the client_roles entries - if description said "both", you MUST have \
2 entries for that client
   - Count the number of times each client appears in your client_roles list
   - If description said "both X and Y" for a client, that client MUST appear EXACTLY 2 times

⚠️⚠️⚠️ CRITICAL OUTPUT FORMAT RULES - READ BEFORE GENERATING JSON ⚠️⚠️⚠️
1. EACH client-role pair MUST be a SEPARATE object in the client_roles array
2. If a realm role needs multiple roles from the same client, create MULTIPLE SEPARATE objects:
  ✅ CORRECT: [{{"client": "service-A", "role": "read"}}, {{"client": "service-A", "role": "write"}}]
  ❌ WRONG: [{{"client": "service-A", "role": ["read", "write"]}}]
  ❌ WRONG: [{{"client": "service-A", "roles": ["read", "write"]}}]
  ❌ WRONG: [{{"client": "service-A", "role": "read, write"}}]
3. The "role" field MUST be a STRING, never an array or comma-separated list
4. Each object has exactly 2 fields: "client" and "role" (both strings)
5. If you need to assign N roles, create N separate objects - one per role

EXAMPLE - When a user needs "both type-X and type-Y access" to "tool-Z":
✅ CORRECT FORMAT:
{{
 "role": "user-group",
 "client_roles": [
   {{"client": "tool-Z", "role": "type-X-access"}},
   {{"client": "tool-Z", "role": "type-Y-access"}}
 ]
}}

❌ WRONG FORMATS (DO NOT USE THESE):
{{"client": "tool-Z", "role": ["type-X-access", "type-Y-access"]}}
{{"client": "tool-Z", "roles": ["type-X-access", "type-Y-access"]}}
{{"client": "tool-Z", "role": "type-X-access, type-Y-access"}}

Return in this format:
```explanation
[Your brief explanation of how you mapped the natural language to preset names, including call chain and role \
assignments. Explicitly mention when you assigned multiple roles for the same client due to "both" or "and" keywords.]
```

```json
[
 {{
   "role": "exact-preset-realm-role-name",
   "client_roles": [
     {{"client": "client-name", "role": "client-role-name"}},
     {{"client": "client-name", "role": "another-client-role-name"}}
   ]
 }}
]
```
"""


def build_retry_prompt(realm_roles: List[Dict[str, str]], client_roles_map: Dict[str, List[Dict[str, str]]]) -> str:
    """
    Build a retry prompt when initial JSON parsing fails.

    This prompt is sent to the LLM when it fails to return valid JSON
    on the first attempt. It provides a simplified reminder of the
    available roles and the expected output format.

    Args:
        realm_roles: List of dicts with 'name' and 'description' for realm roles
        client_roles_map: Dict mapping client names to list of role dicts

    Returns:
        Formatted retry prompt string with role reminders and format example
    """
    # Build a concise summary of realm roles
    realm_role_names = [role["name"] for role in realm_roles]

    # Build a concise summary of available client roles
    client_roles_summary = []
    for client, roles in client_roles_map.items():
        role_names = [role["name"] for role in roles]
        client_roles_summary.append(f"{client}: {', '.join(role_names)}")

    return f"""The previous response could not be parsed as valid JSON.

Please provide the mapping again using ONLY these preset names:
- Realm roles: {", ".join(realm_role_names) if realm_role_names else "(none)"}
- Clients with roles: {"; ".join(client_roles_summary) if client_roles_summary else "(none)"}

Remember: Each realm role should specify which client roles from which clients users need.

Return in this format:
```explanation
[Your brief explanation]
```

```json
[
  {{
    "role": "exact-preset-realm-role-name",
    "client_roles": [
      {{"client": "client-name", "role": "client-role-name"}}
    ]
  }}
]
```"""


# Made with Bob
