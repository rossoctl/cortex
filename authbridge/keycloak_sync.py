#!/usr/bin/env python3
"""
Keycloak Sync - Reconcile routes.yaml with Keycloak configuration.

This script compares the targets defined in routes.yaml against Keycloak
and surfaces any discrepancies for interactive resolution.

Keycloak remains the source of truth - this script only proposes changes.
"""

import argparse
import sys
from dataclasses import dataclass
from typing import Optional

import yaml
from keycloak import KeycloakAdmin


@dataclass
class RouteTarget:
    """A target from routes.yaml that needs reconciliation."""

    host: str
    audience: str
    scopes: list[str]
    passthrough: bool = False


@dataclass
class ReconcileResult:
    """Summary of reconciliation actions."""

    targets_checked: int = 0
    clients_created: int = 0
    clients_skipped: int = 0
    scopes_created: int = 0
    scopes_skipped: int = 0
    scopes_assigned: int = 0
    hostnames_set: int = 0
    hostnames_skipped: int = 0
    agent_client_created: bool = False
    errors: int = 0


class KeycloakReconciler:
    """Reconciles routes.yaml targets with Keycloak configuration."""

    HOSTNAME_ATTRIBUTE = "authbridge.hostname"

    def __init__(
        self,
        keycloak_admin: KeycloakAdmin,
        dry_run: bool = False,
        auto_yes: bool = False,
        agent_client: Optional[str] = None,
    ):
        self.kc = keycloak_admin
        self.dry_run = dry_run
        self.auto_yes = auto_yes
        self.agent_client = agent_client
        self.agent_client_uuid: Optional[str] = None
        self.result = ReconcileResult()

        # Look up agent client UUID if provided
        if agent_client:
            self.agent_client_uuid = self.kc.get_client_id(agent_client)
            if not self.agent_client_uuid:
                print(f"[MISSING] Agent client '{agent_client}' not found in Keycloak")
                if self._prompt(f"  Create agent client '{agent_client}'?"):
                    self.agent_client_uuid = self._create_agent_client(agent_client)
                    if self.agent_client_uuid:
                        self.result.agent_client_created = True
                    else:
                        self.result.errors += 1
                else:
                    print("  --> Skipped")

    def reconcile(self, targets: list[RouteTarget]) -> ReconcileResult:
        """Reconcile all targets against Keycloak."""
        for target in targets:
            if target.passthrough:
                print(f"\n[{target.audience}]")
                print("  - Passthrough route, skipping")
                continue

            self._reconcile_target(target)
            self.result.targets_checked += 1

        return self.result

    def _reconcile_target(self, target: RouteTarget):
        """Reconcile a single target."""
        print(f"\n[{target.audience}]")

        # Step 1: Check client exists
        client_id = self._check_client(target.audience)
        if client_id is None:
            return

        # Step 2: Check scopes
        self._check_scopes(target.audience, target.scopes, client_id)

        # Step 3: Check hostname attribute
        self._check_hostname(target.audience, target.host, client_id)

    def _check_client(self, audience: str) -> Optional[str]:
        """Check if client exists in Keycloak. Returns client UUID or None."""
        client_id = self.kc.get_client_id(audience)

        if client_id:
            print("  [OK] Client exists")
            return client_id

        print(f"  [MISSING] Client '{audience}' not found in Keycloak")
        if self._prompt(f"    Create client '{audience}'?"):
            client_id = self._create_client(audience)
            if client_id:
                self.result.clients_created += 1
                return client_id
            else:
                self.result.errors += 1
                return None
        else:
            print("    --> Skipped")
            self.result.clients_skipped += 1
            return None

    def _check_scopes(self, audience: str, scopes: list[str], _client_id: str):
        """Check that required scopes exist with correct audience mappers."""
        # Find scopes that look like audience scopes (ending in -aud)
        audience_scopes = [s for s in scopes if s.endswith("-aud")]

        for scope_name in audience_scopes:
            scope = self._find_scope(scope_name)

            if scope is None:
                print(f"  [MISSING] Scope '{scope_name}' not found")
                if self._prompt(f"    Create scope '{scope_name}' with audience mapper?"):
                    if self._create_scope_with_mapper(scope_name, audience):
                        self.result.scopes_created += 1
                        # Assign newly created scope to agent
                        scope = self._find_scope(scope_name)
                        if scope and self.agent_client_uuid:
                            self._assign_scope_to_agent(scope_name, scope["id"])
                    else:
                        self.result.errors += 1
                else:
                    print("    --> Skipped")
                    self.result.scopes_skipped += 1
                continue

            print(f"  [OK] Scope '{scope_name}' exists")

            # Check mapper
            mapper = self._find_audience_mapper(scope["id"])
            if mapper is None:
                print(f"  [WARN] Scope '{scope_name}' has no audience mapper")
                if self._prompt(f"    Add audience mapper for '{audience}'?"):
                    self._add_audience_mapper(scope["id"], scope_name, audience)
            else:
                print("  [OK] Audience mapper correctly configured")

            # Assign scope to agent client if specified
            if self.agent_client_uuid:
                self._assign_scope_to_agent(scope_name, scope["id"])

    def _check_hostname(self, _audience: str, expected_host: str, client_id: str):
        """Check that the client has the correct hostname attribute."""
        client = self.kc.get_client(client_id)
        attributes = client.get("attributes", {})
        current_host = attributes.get(self.HOSTNAME_ATTRIBUTE)

        if current_host is None:
            print("  [WARN] No hostname attribute set")
            if self._prompt(f"    Set hostname to '{expected_host}'?"):
                self._set_hostname_attribute(client_id, expected_host)
                self.result.hostnames_set += 1
            else:
                print("    --> Skipped")
                self.result.hostnames_skipped += 1
        elif current_host != expected_host:
            print(f"  [WARN] Hostname is '{current_host}', config says '{expected_host}'")
            if self._prompt(f"    Update hostname to '{expected_host}'?"):
                self._set_hostname_attribute(client_id, expected_host)
                self.result.hostnames_set += 1
            else:
                print("    --> Skipped")
                self.result.hostnames_skipped += 1
        else:
            print("  [OK] Hostname attribute matches")

    # --- Keycloak operations ---

    def _create_client(self, client_id: str) -> Optional[str]:
        """Create a new client in Keycloak."""
        if self.dry_run:
            print(f"    --> [DRY RUN] Would create client '{client_id}'")
            return "dry-run-id"

        try:
            payload = {
                "clientId": client_id,
                "name": client_id,
                "enabled": True,
                "publicClient": False,
                "standardFlowEnabled": False,
                "serviceAccountsEnabled": False,
            }
            self.kc.create_client(payload)
            uuid = self.kc.get_client_id(client_id)
            print(f"    --> Created client '{client_id}'")
            return uuid
        except Exception as e:
            print(f"    --> Error creating client: {e}")
            return None

    def _create_agent_client(self, client_id: str) -> Optional[str]:
        """Create an agent client in Keycloak with appropriate settings."""
        if self.dry_run:
            print(f"  --> [DRY RUN] Would create agent client '{client_id}'")
            return "dry-run-id"

        try:
            payload = {
                "clientId": client_id,
                "name": client_id,
                "enabled": True,
                "publicClient": False,
                "standardFlowEnabled": True,
                "directAccessGrantsEnabled": True,
                "serviceAccountsEnabled": True,
                "fullScopeAllowed": False,
                "attributes": {
                    "standard.token.exchange.enabled": "true",
                },
            }
            self.kc.create_client(payload)
            uuid = self.kc.get_client_id(client_id)
            print(f"  --> Created agent client '{client_id}'")

            # Note: Do not print the client secret to stdout to avoid leaking credentials.
            # The secret can be retrieved securely via the Keycloak admin console or API.

            return uuid
        except Exception as e:
            print(f"  --> Error creating agent client: {e}")
            return None

    def _find_scope(self, scope_name: str) -> Optional[dict]:
        """Find a client scope by name."""
        scopes = self.kc.get_client_scopes()
        for scope in scopes:
            if scope["name"] == scope_name:
                return scope
        return None

    def _find_audience_mapper(self, scope_id: str) -> Optional[dict]:
        """Find an audience mapper in a scope."""
        try:
            mappers = self.kc.get_mappers_from_client_scope(scope_id)
            for mapper in mappers:
                if mapper.get("protocolMapper") == "oidc-audience-mapper":
                    return mapper
        except Exception:
            pass
        return None

    def _create_scope_with_mapper(self, scope_name: str, audience: str) -> bool:
        """Create a client scope with an audience mapper."""
        if self.dry_run:
            print(f"    --> [DRY RUN] Would create scope '{scope_name}'")
            return True

        try:
            scope_payload = {
                "name": scope_name,
                "protocol": "openid-connect",
                "attributes": {
                    "include.in.token.scope": "true",
                    "display.on.consent.screen": "false",
                },
            }
            self.kc.create_client_scope(scope_payload)
            scope = self._find_scope(scope_name)
            if scope:
                self._add_audience_mapper(scope["id"], scope_name, audience)
                print(f"    --> Created scope '{scope_name}' with audience mapper")
                return True
        except Exception as e:
            print(f"    --> Error creating scope: {e}")
        return False

    def _add_audience_mapper(self, scope_id: str, mapper_name: str, audience: str):
        """Add an audience mapper to a scope."""
        if self.dry_run:
            print("    --> [DRY RUN] Would add audience mapper")
            return

        mapper_payload = {
            "name": mapper_name,
            "protocol": "openid-connect",
            "protocolMapper": "oidc-audience-mapper",
            "consentRequired": False,
            "config": {
                "included.custom.audience": audience,
                "id.token.claim": "false",
                "access.token.claim": "true",
                "userinfo.token.claim": "false",
            },
        }
        try:
            self.kc.add_mapper_to_client_scope(scope_id, mapper_payload)
            print("    --> Added audience mapper")
        except Exception as e:
            print(f"    --> Error adding mapper: {e}")

    def _set_hostname_attribute(self, client_id: str, hostname: str):
        """Set the hostname attribute on a client."""
        if self.dry_run:
            print(f"    --> [DRY RUN] Would set hostname to '{hostname}'")
            return

        try:
            client = self.kc.get_client(client_id)
            attributes = client.get("attributes", {})
            attributes[self.HOSTNAME_ATTRIBUTE] = hostname
            self.kc.update_client(client_id, {"attributes": attributes})
            print("    --> Set hostname attribute")
        except Exception as e:
            print(f"    --> Error setting hostname: {e}")

    def _assign_scope_to_agent(self, scope_name: str, scope_id: str):
        """Assign a scope to the agent client as an optional scope."""
        if not self.agent_client_uuid:
            return

        # Check if already assigned
        try:
            optional_scopes = self.kc.get_client_optional_client_scopes(self.agent_client_uuid)
            for s in optional_scopes:
                if s.get("name") == scope_name:
                    print(f"  [OK] Scope '{scope_name}' already assigned to agent")
                    return
        except Exception:
            # If we cannot retrieve existing optional scopes, continue and attempt
            # to assign the scope anyway; the subsequent call will handle conflicts.
            pass

        if self.dry_run:
            print(f"    --> [DRY RUN] Would assign scope '{scope_name}' to agent")
            return

        try:
            self.kc.add_client_optional_client_scope(self.agent_client_uuid, scope_id, {})
            print(f"  --> Assigned scope '{scope_name}' to agent")
            self.result.scopes_assigned += 1
        except Exception as e:
            if "already exists" in str(e).lower() or "409" in str(e):
                print(f"  [OK] Scope '{scope_name}' already assigned to agent")
            else:
                print(f"    --> Error assigning scope to agent: {e}")

    # --- Helpers ---

    def _prompt(self, message: str) -> bool:
        """Prompt user for confirmation. Returns True if yes."""
        if self.auto_yes:
            print(f"{message} [y/N]: y (auto)")
            return True

        try:
            response = input(f"{message} [y/N]: ").strip().lower()
            return response in ("y", "yes")
        except EOFError:
            return False


def _prompt_user(message: str) -> bool:
    """Prompt user for confirmation. Returns True if yes."""
    try:
        response = input(f"{message} [y/N]: ").strip().lower()
        return response in ("y", "yes")
    except EOFError:
        return False


def load_routes(path: str) -> list[RouteTarget]:
    """Load routes from YAML file and convert to RouteTarget objects."""
    with open(path) as f:
        routes = yaml.safe_load(f) or []

    targets = []
    for route in routes:
        if not route.get("target_audience"):
            continue  # Skip routes without audience (e.g., passthrough-only)

        scopes = route.get("token_scopes", "").split()
        targets.append(
            RouteTarget(
                host=route.get("host", ""),
                audience=route["target_audience"],
                scopes=scopes,
                passthrough=route.get("passthrough", False),
            )
        )

    return targets


def print_summary(result: ReconcileResult):
    """Print reconciliation summary."""
    print("\n" + "=" * 50)
    print("Summary:")
    print(f"  {result.targets_checked} targets checked")
    if result.agent_client_created:
        print("  Agent client created")
    if result.clients_created:
        print(f"  {result.clients_created} clients created")
    if result.clients_skipped:
        print(f"  {result.clients_skipped} clients skipped (not in Keycloak)")
    if result.scopes_created:
        print(f"  {result.scopes_created} scopes created")
    if result.scopes_skipped:
        print(f"  {result.scopes_skipped} scopes skipped")
    if result.scopes_assigned:
        print(f"  {result.scopes_assigned} scopes assigned to agent")
    if result.hostnames_set:
        print(f"  {result.hostnames_set} hostname attributes set")
    if result.hostnames_skipped:
        print(f"  {result.hostnames_skipped} hostname attributes skipped")
    if result.errors:
        print(f"  {result.errors} errors encountered")


def main():
    parser = argparse.ArgumentParser(description="Reconcile routes.yaml with Keycloak configuration")
    parser.add_argument(
        "--config",
        "-c",
        default="/etc/authproxy/routes.yaml",
        help="Path to routes.yaml (default: /etc/authproxy/routes.yaml)",
    )
    parser.add_argument("--keycloak-url", default="http://keycloak.localtest.me:8080", help="Keycloak server URL")
    parser.add_argument("--realm", default="rossoctl", help="Keycloak realm (default: rossoctl)")
    parser.add_argument("--admin-user", default="admin", help="Keycloak admin username")
    parser.add_argument("--admin-password", default="admin", help="Keycloak admin password")
    parser.add_argument("--dry-run", "-n", action="store_true", help="Show what would be done without making changes")
    parser.add_argument("--yes", "-y", action="store_true", help="Answer yes to all prompts")
    parser.add_argument("--agent-client", help="Client ID of the agent to assign scopes to (optional)")

    args = parser.parse_args()

    # Load routes
    print(f"Loading routes from {args.config}...")
    try:
        targets = load_routes(args.config)
    except Exception as e:
        print(f"Error loading routes: {e}")
        sys.exit(1)

    if not targets:
        print("No targets found in routes config")
        sys.exit(0)

    print(f"Found {len(targets)} targets to reconcile")

    # Connect to Keycloak master realm first to check if target realm exists
    print(f"Connecting to Keycloak at {args.keycloak_url}...")
    try:
        master_kc = KeycloakAdmin(
            server_url=args.keycloak_url,
            username=args.admin_user,
            password=args.admin_password,
            realm_name="master",
            user_realm_name="master",
        )
    except Exception as e:
        print(f"Error connecting to Keycloak: {e}")
        sys.exit(1)

    # Check if realm exists, prompt to create if not
    realm_exists = False
    try:
        realms = master_kc.get_realms()
        realm_exists = any(r.get("realm") == args.realm for r in realms)
    except Exception as e:
        print(f"Error checking realms: {e}")
        sys.exit(1)

    if not realm_exists:
        print(f"[MISSING] Realm '{args.realm}' not found")
        if args.yes or _prompt_user(f"  Create realm '{args.realm}'?"):
            if args.dry_run:
                print(f"  --> [DRY RUN] Would create realm '{args.realm}'")
            else:
                try:
                    master_kc.create_realm(
                        {
                            "realm": args.realm,
                            "enabled": True,
                            "displayName": args.realm,
                        }
                    )
                    print(f"  --> Created realm '{args.realm}'")
                except Exception as e:
                    print(f"  --> Error creating realm: {e}")
                    sys.exit(1)
        else:
            print("  --> Skipped (realm required)")
            sys.exit(1)

    # Connect to target realm
    try:
        kc = KeycloakAdmin(
            server_url=args.keycloak_url,
            username=args.admin_user,
            password=args.admin_password,
            realm_name=args.realm,
            user_realm_name="master",
        )
    except Exception as e:
        print(f"Error connecting to realm '{args.realm}': {e}")
        sys.exit(1)

    # Reconcile
    if args.dry_run:
        print("\n[DRY RUN MODE - no changes will be made]\n")

    if args.agent_client:
        print(f"Agent client: {args.agent_client}")

    reconciler = KeycloakReconciler(kc, dry_run=args.dry_run, auto_yes=args.yes, agent_client=args.agent_client)
    result = reconciler.reconcile(targets)

    print_summary(result)


if __name__ == "__main__":
    main()
