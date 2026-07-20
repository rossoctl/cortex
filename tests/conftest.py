"""Shared fixtures for cortex tests."""

from unittest.mock import MagicMock

import pytest


@pytest.fixture
def mock_keycloak_admin():
    """Create a mock KeycloakAdmin instance with common methods stubbed."""
    admin = MagicMock()
    admin.get_client_id.return_value = None
    admin.get_client_scopes.return_value = []
    admin.get_client_secrets.return_value = {"value": "test-secret-value"}
    return admin


@pytest.fixture
def tmp_secret_file(tmp_path):
    """Provide a temporary file path for secret writing tests."""
    return str(tmp_path / "secret.txt")
