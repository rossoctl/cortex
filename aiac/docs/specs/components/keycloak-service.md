# ~~Component PRD: Keycloak Configuration Service~~

> **Superseded.** This component has been replaced by two separate services:
> - **IdP Configuration Service** (read endpoints) — see [idp-configuration-service.md](idp-configuration-service.md)
> - **PDP Policy Writer — Keycloak Implementation** (write endpoints) — see [pdp-policy-keycloak-service.md](pdp-policy-keycloak-service.md)
>
> The content below is retained for reference only.

## Location
`aiac/src/aiac/keycloak/service/`

## Description
A FastAPI web service that proxies Keycloak Admin REST API endpoints. Returns raw Keycloak JSON unchanged for read operations; forwards write operations directly. Stateless — no caching.

## Endpoints

| Method | Path | Keycloak Admin API call | Description |
|--------|------|------------------------|-------------|
| GET | `/users` | `GET /admin/realms/{realm}/users` | All users in realm |
| GET | `/roles` | `GET /admin/realms/{realm}/roles` | All realm-level roles |
| GET | `/users/{user_id}/role-mappings` | `GET /admin/realms/{realm}/users/{user_id}/role-mappings` | Realm and client role mappings for a user |
| GET | `/clients` | `GET /admin/realms/{realm}/clients` | All clients |
| GET | `/client-scopes` | `GET /admin/realms/{realm}/client-scopes` | All client scopes |
| GET | `/clients/{client_id}/roles` | `GET /admin/realms/{realm}/clients/{client_id}/roles` | Roles defined for a specific client |
| POST | `/users/{user_id}/role-mappings/clients/{client_id}` | `POST /admin/realms/{realm}/users/{user_id}/role-mappings/clients/{client_id}` | Assign client roles to a user |
| DELETE | `/users/{user_id}/role-mappings/clients/{client_id}` | `DELETE /admin/realms/{realm}/users/{user_id}/role-mappings/clients/{client_id}` | Revoke client roles from a user |
| POST | `/clients/{client_id}/roles` | `POST /admin/realms/{realm}/clients/{client_id}/roles` | Create a new role for a specific client |
| POST | `/clients/{client_id}/scopes` | `POST /admin/realms/{realm}/client-scopes` then `PUT /admin/realms/{realm}/clients/{client_id}/default-client-scopes/{scope_id}` | Create a realm-level scope and assign it to a client as a default scope (atomic) |
| DELETE | `/role-mappings` | loop: `GET /admin/realms/{realm}/users` → per user `GET /admin/realms/{realm}/users/{id}/role-mappings` → `DELETE /admin/realms/{realm}/users/{id}/role-mappings/clients/{client_id}` per client | Revoke all client role assignments for all users in the realm |

Every endpoint accepts an optional `realm` query parameter. When supplied, the request targets the named Keycloak realm instead of the service default (`KEYCLOAK_REALM`); a new `KeycloakAdmin` bound to that realm is instantiated per request. When omitted, the singleton admin initialised at startup is used.

The GET endpoints return `200 OK` with a JSON array on success, except `/users/{user_id}/role-mappings` which returns a JSON object with `realmMappings` and `clientMappings` fields. The POST and DELETE endpoints for role-mapping operations accept a JSON array of role representation objects in the request body and return `204 No Content` on success. `POST /clients/{client_id}/roles` and `POST /clients/{client_id}/scopes` accept a JSON object with `name` and `description` fields and return `201 Created` with the created resource as JSON. `DELETE /role-mappings` returns `204 No Content` on success. All endpoints return `502 Bad Gateway` with a JSON error body if the Keycloak Admin API call fails.

## Configuration

Environment variables (injected via Kubernetes Deployment manifest):

| Variable | Required | Description |
|----------|----------|-------------|
| `KEYCLOAK_URL` | Yes | Keycloak base URL, e.g. `http://keycloak-service.keycloak.svc:8080` |
| `KEYCLOAK_REALM` | Yes | Realm name, e.g. `kagenti` |
| `KEYCLOAK_ADMIN_USERNAME` | Yes | Admin username (from `keycloak-admin-secret`) |
| `KEYCLOAK_ADMIN_PASSWORD` | Yes | Admin password (from `keycloak-admin-secret`) |

## Runtime

- Framework: FastAPI
- Server: uvicorn
- Bind: `0.0.0.0:7070`
- Base image: `python:3.14-slim`

## Dependencies (`requirements.txt`)

```
fastapi
uvicorn[standard]
python-keycloak
```

## File structure

```
aiac/src/aiac/keycloak/service/
├── Dockerfile
├── requirements.txt
└── main.py
```

## `main.py` behaviour notes

- Instantiate the default `KeycloakAdmin` once at startup using env vars.
- `get_admin` is a FastAPI dependency accepting `realm: str | None = Query(None)`. When `realm` is `None` it returns the startup singleton; when `realm` is set it returns a new `KeycloakAdmin` for that realm.
- Each endpoint declares `admin: KeycloakAdmin = Depends(get_admin)` — no per-route changes needed for realm routing.
- Each GET endpoint calls the corresponding `python-keycloak` method and returns the result directly via `JSONResponse`.
- POST `/users/{user_id}/role-mappings/clients/{client_id}`: assign the provided roles and return `Response(status_code=204)`.
- DELETE `/users/{user_id}/role-mappings/clients/{client_id}`: revoke the provided roles and return `Response(status_code=204)`.
- POST `/clients/{client_id}/roles`: call `admin.create_client_role(client_id, {"name": ..., "description": ...})`; return `JSONResponse(status_code=201)` with the created role representation.
- POST `/clients/{client_id}/scopes`: call `admin.create_client_scope({"name": ..., "description": ..., "protocol": "openid-connect"})` to create the realm-level scope, then call `admin.add_default_default_client_scope(client_id, scope_id)` to assign it to the client; return `JSONResponse(status_code=201)` with the created scope representation.
- DELETE `/role-mappings`: fetch all users; for each user fetch role mappings; for each client key in `clientMappings` call `admin.delete_client_roles_of_user(user_id, client_id, roles)`; return `Response(status_code=204)` when all revocations complete.
- On `KeycloakError`, return HTTP 502 with `{"error": str(e)}`.
