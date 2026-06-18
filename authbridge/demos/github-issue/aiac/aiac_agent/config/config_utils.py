#!/usr/bin/env python3
"""
Configuration Utilities for Policy Builder

This module provides utilities for loading and parsing configuration files
that define realm roles, clients, and call chain relationships.
"""

from pathlib import Path
from typing import Any, Dict, List

import yaml


def load_config(config_path: Path) -> Dict[str, Any]:
    """
    Load configuration from YAML config file.

    Args:
        config_path: Path to the YAML configuration file

    Returns:
        Dictionary containing configuration data including realm_roles,
        clients, and client_audience_targets

    Raises:
        FileNotFoundError: If config file doesn't exist
        yaml.YAMLError: If config file is invalid YAML
    """
    with open(config_path, "r") as f:
        return yaml.safe_load(f)


def extract_realm_roles_and_clients(
    config: Dict[str, Any],
) -> tuple[List[Dict[str, str]], Dict[str, List[Dict[str, str]]], Dict[str, List[str]]]:
    """
    Extract realm roles, clients with their roles, and client call chains from config.

    This function parses the configuration to extract three key pieces of information:
    1. Realm roles (user roles in Keycloak) with descriptions
    2. Client roles map (which roles each client has) with descriptions
    3. Client audience targets (call chain relationships)

    Supports both old format (list of strings) and new format (list of dicts with name/description).

    Args:
        config: Configuration dictionary loaded from YAML

    Returns:
        Tuple containing:
            - realm_roles: List of dicts with 'name' and optional 'description'
            - client_roles_map: Dict mapping client_name -> [{'name': role, 'description': desc}, ...]
            - client_audience_targets: Dict mapping client -> [target_clients]

    Example:
        >>> config = {
        ...     'realm_roles': [{'name': 'admin', 'description': 'Administrator'}],
        ...     'clients': [{'client_id': 'app', 'roles': [{'name': 'read', 'description': 'Read access'}]}],
        ...     'client_audience_targets': {'app': ['api']}
        ... }
        >>> roles, clients, targets = extract_realm_roles_and_clients(config)
        >>> roles
        [{'name': 'admin', 'description': 'Administrator'}]
        >>> clients
        {'app': [{'name': 'read', 'description': 'Read access'}]}
    """
    # Extract realm roles (user roles) - support both old and new format
    realm_roles_raw = config.get("realm_roles", [])
    realm_roles = []
    for role in realm_roles_raw:
        if isinstance(role, dict):
            # New format with name and description
            realm_roles.append({"name": role["name"], "description": role.get("description", "")})
        else:
            # Old format - just role name as string
            realm_roles.append({"name": role, "description": ""})

    # Extract client names and their roles from clients list
    clients = config.get("clients", [])
    client_roles_map = {}

    for client in clients:
        if "client_id" in client:
            client_id = client["client_id"]
            roles_raw = client.get("roles", [])

            # Parse roles - support both old and new format
            roles = []
            for role in roles_raw:
                if isinstance(role, dict):
                    # New format with name and description
                    roles.append({"name": role["name"], "description": role.get("description", "")})
                else:
                    # Old format - just role name as string
                    roles.append({"name": role, "description": ""})

            client_roles_map[client_id] = roles

    # Extract client audience targets (call chains)
    # This defines which clients can call which other clients
    client_audience_targets = config.get("client_audience_targets", {})

    return realm_roles, client_roles_map, client_audience_targets
