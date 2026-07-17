# Component PRD: PDP Policy Writer — Keycloak Implementation

## Location
`aiac/src/aiac/pdp/service/policy/keycloak/`

## Description
A FastAPI web service that applies RBAC policy changes to Keycloak by managing composite role mappings. Roles are made composites of service (client) permissions (roles), so that any subject (user) assigned a role automatically inherits the associated service permissions. Stateless — no caching.

This is the **Phase 1** implementation of the PDP Policy Writer. It is deployed as a container in the **Kagenti Interface Pod** alongside the IdP Configuration Service, behind the `aiac-pdp-policy-service:7072` ClusterIP. Phase 2 replaces only this container image (`aiac-pdp-policy-keycloak` → `aiac-pdp-policy-opa`) within the same pod. The service name and port remain stable so the AIAC Agent and library require no reconfiguration.

## Endpoints

| Method | Path | Keycloak Admin API call | Description |
|--------|------|------------------------|-------------|
| POST | `/roles/{role_name}/composites` | `POST /admin/realms/{realm}/roles/{role-name}/composites` | Add service permissions (client roles) to a role composite |
| DELETE | `/roles/{role_name}/composites` | `DELETE /admin/realms/{realm}/roles/{role-name}/composites` | Remove service permissions (client roles) from a role composite |
| DELETE | `/composites` | loop: `GET /admin/realms/{realm}/roles` → per role `GET .../composites` → `DELETE .../composites` | Revoke all composite mappings from all roles (rebuild) |
| POST | `/services/{service_id}/permissions` | `POST /admin/realms/{realm}/clients/{service_id}/roles` | Create a new permission (client role) for a specific service |
| POST | `/services/{service_id}/scopes` | `POST /admin/realms/{realm}/client-scopes` then `PUT .../clients/{service_id}/default-client-scopes/{scope_id}` | Create a realm-level scope and assign it to a service as a default scope (atomic) |

Every endpoint accepts an optional `realm` query parameter, same as the IdP Configuration Service.

`POST /roles/{role_name}/composites` and `DELETE /roles/{role_name}/composites` accept a JSON array of role representation objects `[{"id": "...", "name": "..."}]` and return `204 No Content` on success.

`DELETE /composites` returns `204 No Content` on success.

`POST /services/{service_id}/permissions` and `POST /services/{service_id}/scopes` accept a JSON object `{"name": "...", "description": "..."}` and return `201 Created` with the created resource as JSON.

All endpoints return `502 Bad Gateway` with a JSON error body if the Keycloak Admin API call fails.

## Configuration

| Variable | Required | Description |
|----------|----------|-------------|
| `KEYCLOAK_URL` | Yes | Keycloak base URL, e.g. `http://keycloak-service.keycloak.svc:8080` |
| `KEYCLOAK_REALM` | Yes | Realm name, e.g. `kagenti` |
| `KEYCLOAK_ADMIN_USERNAME` | Yes | Admin username (from `keycloak-admin-secret`) |
| `KEYCLOAK_ADMIN_PASSWORD` | Yes | Admin password (from `keycloak-admin-secret`) |

## Runtime

- Framework: FastAPI
- Server: uvicorn
- Bind: `0.0.0.0:7072`
- Base image: `python:3.12-slim`
- Kubernetes ClusterIP Service: `aiac-pdp-policy-service:7072`
- Deployment: co-located with IdP Configuration Service as a container in the **Kagenti Interface Pod** (`pdp-interface-deployment.yaml`)

## Dependencies (`requirements.txt`)

```
fastapi
uvicorn[standard]
python-keycloak
```

## File structure

```
aiac/src/aiac/pdp/service/
├── __init__.py
└── policy/
    ├── __init__.py
    └── keycloak/
        ├── __init__.py
        ├── Dockerfile
        ├── requirements.txt
        └── main.py
```

## `main.py` behaviour notes

- Instantiate the default `KeycloakAdmin` once at startup using env vars.
- `get_admin` is a FastAPI dependency accepting `realm: str | None = Query(None)`.
- `POST /roles/{role_name}/composites`: call `admin.add_composite_realm_roles_to_role(role_name, roles)`; return `Response(status_code=204)`.
- `DELETE /roles/{role_name}/composites`: call `admin.remove_composite_realm_roles_to_role(role_name, roles)`; return `Response(status_code=204)`.
- `DELETE /composites`: fetch all roles via `admin.get_realm_roles()`; for each role call `admin.get_composite_realm_roles_of_role(role_name)`; if composites are non-empty call `admin.remove_composite_realm_roles_to_role(role_name, composites)`; return `Response(status_code=204)`.
- `POST /services/{service_id}/permissions`: call `admin.create_client_role(service_id, {"name": ..., "description": ...})`; return `JSONResponse(status_code=201)` with the created permission representation.
- `POST /services/{service_id}/scopes`: call `admin.create_client_scope({"name": ..., "description": ..., "protocol": "openid-connect"})` to create the realm-level scope, then call `admin.add_default_default_client_scope(service_id, scope_id)` to assign it to the service; return `JSONResponse(status_code=201)` with the created scope representation.
- On `KeycloakError`, return HTTP 502 with `{"error": str(e)}`.
