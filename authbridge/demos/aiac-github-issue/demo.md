# AI Access Control (AIAC) Demo for GitHub Issue Agent with AuthBridge

This demo showcases **AI based access control policy generation** integrated with
**AuthBridge** and **Keycloak**. It demonstrates how natural language policy descriptions
can be automatically converted into structured YAML policies and applied to a Keycloak
realm for role-based access control (RBAC).

This demo extends the [GitHub Issue Agent demo](../github-issue/demo.md) by adding
AI-driven policy management capabilities.

## What This Demo Shows

1. **Natural Language Policy Generation** — Convert plain English policy descriptions into structured YAML
2. **LLM-Based Policy Mapping** — Use AIAC agent to map user roles to client roles with semantic understanding
3. **Automated Policy Validation** — Structural and semantic validation with automatic retry
4. **Keycloak Integration** — Export realm configuration, generate policies, and apply them automatically
5. **Policy-as-Code** — Declarative access control policies
6. **Composite Role Management** — Automatic creation of composite role mappings in Keycloak

## Architecture Overview

```
Natural Language Policy Description
         │
         ▼
   PolicyBuilder (AIAC agent)
    ┌─────────────────────┐
    │ 1. Parse & Extract  │ ◄── LLM (GPT-4, Claude, Ollama, etc.)
    │ 2. Build Policy     │
    │ 3. Generate YAML    │
    │ 4. Validate         │
    └─────────────────────┘
         │
         ▼
   YAML Policy File
         │
         ▼
   Keycloak Operations
    ┌─────────────────────┐
    │ 1. Export Config    │
    │ 2. Delete Old Policy│
    │ 3. Apply New Policy │
    └─────────────────────┘
         │
         ▼
   Keycloak Realm (Updated)
```

## Key Components

### 1. AIAC Agent (`aiac_agent/`)
The core policy generation system built with LangGraph:
- **PolicyBuilder**: Main orchestrator class
- **LangGraph Workflow**: Multi-stage state machine (parse → build → generate → validate)
- **LLM Integration**: Supports OpenAI, Ollama, RITS, and other backends
- **Validation**: Structural and semantic policy verification with retry logic

### 2. Keycloak Operations (`keycloak_ops/`)
- **`export_config.py`**: Export realm structure to YAML
- **`apply_policy.py`**: Apply policies as composite role mappings
- **`delete_policy.py`**: Clean up existing policy mappings

### 3. CLI Tools
- **`aiac_cli.py`**: End-to-end pipeline (export → generate → delete → apply)


## Demo Walkthrough

This walkthrough demonstrates the complete AIAC workflow from setup to policy generation and application.

### Prerequisites

Before starting, ensure you have:

- Python 3.9+ with `venv` support
- Keycloak running and accessible
- The kagenti-extensions repository cloned
- Basic understanding of Keycloak concepts (realms, clients, roles)

**Creating GitHub Personal Access Tokens**

Follow GitHub's instructions to create fine-grained PAT tokens:

    <PUBLIC_ACCESS_PAT> — select Public Repositories (read-only) access
    <PRIVILEGED_ACCESS_PAT> — select All Repositories access

This lets you demonstrate finer-grained authorization: a user with full access can see issues on all repositories, while a user with partial access can only see issues on public repositories.

**Build and Load Container Images (if not already done)**

The agent and tool container images must be built locally and loaded into the kind cluster (they are not published to a public registry):
```
cd <path-to>/agent-examples

# Build the GitHub tool image
docker build -t ghcr.io/kagenti/agent-examples/github-tool:latest \
  -f mcp/github_tool/Dockerfile mcp/github_tool/

# Build the GitHub Issue Agent image
docker build -t ghcr.io/kagenti/agent-examples/git-issue-agent:latest \
  -f a2a/git_issue_agent/Dockerfile a2a/git_issue_agent/

# Load both images into the kind cluster
kind load docker-image --name kagenti ghcr.io/kagenti/agent-examples/github-tool:latest
kind load docker-image --name kagenti ghcr.io/kagenti/agent-examples/git-issue-agent:latest
```

### Step 1: Environment Setup

Create and activate a Python virtual environment:

```bash
cd kagenti-extensions/authbridge/demos/aiac-github-issue

# Create virtual environment
python -m venv venv
source venv/bin/activate  # On Windows: venv\Scripts\activate

# Install dependencies
pip install --upgrade pip
pip install -r requirements.txt


### Step 2: Apply Demo ConfigMaps

The Kagenti installer creates default ConfigMaps (`authbridge-config`, `spiffe-helper-config`, `envoy-config`) and the `keycloak-admin-secret` Secret in the target namespace with the correct `kagenti` realm settings.

Apply the demo-specific ConfigMaps — the `authproxy-routes` ConfigMap configures per-route token exchange (target audience and scopes for the `github-tool` host), and `authbridge-config` sets the agent's SPIFFE ID for inbound audience validation. Apply this **before** deploying the agent.

```bash
cd kagenti-extensions/authbridge

# Create namespace if it doesn't exist
kubectl create namespace team1 --dry-run=client -o yaml | kubectl apply -f -

# Apply demo ConfigMaps (authbridge-config and authproxy-routes)
kubectl apply -f demos/aiac-github-issue/k8s/configmaps.yaml -n team1
```

> **Note:** If you're using a different namespace, edit `configmaps.yaml` and update the `namespace` field, or set `AUTHBRIDGE_NAMESPACE=<your-namespace>` and update all subsequent commands accordingly.

> **Keycloak Admin Credentials:** If your Keycloak admin credentials differ from the default (`admin`/`admin`), update the secret:
> ```bash
> kubectl create secret generic keycloak-admin-secret -n team1 \
>   --from-literal=KEYCLOAK_ADMIN_USERNAME=<your-admin-user> \
>   --from-literal=KEYCLOAK_ADMIN_PASSWORD=<your-admin-password> \
>   --dry-run=client -o yaml | kubectl apply -f -
> ```

### Step 3: Create GitHub PAT Secret

The GitHub tool needs Personal Access Tokens (PATs) to access the GitHub API. Create a Kubernetes secret with your tokens:

```bash
# Set your GitHub PAT tokens
export PRIVILEGED_ACCESS_PAT=<your-privileged-pat>
export PUBLIC_ACCESS_PAT=<your-public-pat>

# Create the secret
kubectl create secret generic github-tool-secrets -n team1 \
  --from-literal=INIT_AUTH_HEADER="Bearer $PRIVILEGED_ACCESS_PAT" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer $PRIVILEGED_ACCESS_PAT" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer $PUBLIC_ACCESS_PAT"
```

**Creating GitHub Personal Access Tokens:**

Follow [GitHub's instructions](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token) to create fine-grained PAT tokens:

- **`PRIVILEGED_ACCESS_PAT`** — select **All Repositories** access
- **`PUBLIC_ACCESS_PAT`** — select **Public Repositories (read-only)** access

This enables fine-grained authorization: users with full access can see issues on all repositories, while users with partial access can only see issues on public repositories.



### Step 4: Deploy the GitHub Tool

Deploy the GitHub MCP tool as a target service. This deployment does **not** get AuthBridge injection (it is the target, not the caller):

```bash
kubectl apply -f demos/aiac-github-issue/k8s/github-tool-deployment.yaml -n team1

# Wait for the tool to be ready
kubectl wait --for=condition=available --timeout=120s deployment/github-tool -n team1
```

### Step 5: Deploy the GitHub Issue Agent

Deploy the agent with AuthBridge labels. The webhook will automatically inject one combined AuthBridge sidecar. In envoy-sidecar mode it also injects a `proxy-init` init container for iptables setup:

```bash
kubectl apply -f demos/aiac-github-issue/k8s/git-issue-agent-deployment.yaml -n team1

# Wait for the agent to be ready (may take longer due to client registration)
kubectl wait --for=condition=available --timeout=180s deployment/git-issue-agent -n team1
```

> **Note:** The agent may take longer to start because it waits on `/shared/client-{id,secret}.txt` to be populated by the operator's `ClientRegistrationReconciler` before the AuthBridge sidecar becomes ready.

**Verify injected containers:**

Confirm that the webhook injected the combined AuthBridge sidecar:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected (proxy-sidecar mode, the cluster default):
```
agent authbridge-proxy spiffe-helper
```

Or, in envoy-sidecar mode:
```
agent envoy-proxy spiffe-helper
```

### Step 6: Validate the Deployment

#### Check pod status

```bash
kubectl get pods -n team1
```

Expected output:
```
NAME                               READY   STATUS    RESTARTS   AGE
git-issue-agent-xxxxxxxxxx-xxxxx   3/3     Running   0          2m
github-tool-yyyyyyyyyyy-yyyyy       1/1     Running   0          3m
```

#### Check operator-managed client registration

After the operator registers the client, verify the resulting Secret was mounted into the agent's sidecar:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.volumes[?(@.secret)].secret.secretName}'
```

Expected: A Secret name starting with `kagenti-keycloak-client-credentials-....`

**Follow the operator-side registration:**

```bash
kubectl logs -n kagenti-system deployment/kagenti-controller-manager \
  | grep -iE "clientregistration|git-issue-agent" | tail -20
```

Expected (operator log lines):
```
ClientRegistrationReconciler: ensured Keycloak client
  spiffe://localtest.me/ns/team1/sa/git-issue-agent
ClientRegistrationReconciler: wrote Secret
  kagenti-keycloak-client-credentials-<hex8>
```

#### Check agent logs

```bash
kubectl logs deployment/git-issue-agent -n team1 -c agent
```

Expected:
```
SVID JWT file /opt/jwt_svid.token not found.
CLIENT_SECRET file not found at /shared/secret.txt
INFO: JWKS_URI is set - using JWT Validation middleware
INFO:     Started server process [17]
INFO:     Waiting for application startup.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

> **These warnings are expected and harmless.** The agent's built-in auth code probes for SVID and client-secret files at startup. With AuthBridge, these files are produced and consumed inside the AuthBridge sidecar, not by the agent container directly. The agent falls back to JWKS-based JWT validation (`JWKS_URI is set`), which is the correct behavior.

#### Verify LLM is configured

The agent uses an LLM for inference. Ensure your LLM is accessible:

**For Ollama (local):**
```bash
# Pull the model
ollama pull ibm/granite4:latest

# List available models
ollama list

# Start Ollama server (if not already running)
ollama serve
```

> **Tip:** If using a different model, update `TASK_MODEL_ID` in `git-issue-agent-deployment.yaml` before deploying.

**For OpenAI or other cloud LLMs:**
Ensure your API keys are properly configured in the agent's environment variables.

### Step 7: Test the AuthBridge Flow

Now test that the agent is properly secured with AuthBridge and can communicate with the GitHub tool.

#### Setup test client

Deploy a test client pod to send requests:

```bash
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s
```
Open bash inside the test client pod
```bash
kubectl exec -it test-client -n team1 -- bash
```

#### Test 1: Agent Card (Public Endpoint - No Token Required)

```bash
# run inside test client pod
curl -s http://git-issue-agent:8080/.well-known/agent.json | jq .
```

Expected :
```
Agent card information (publicly accessible)
```

#### Test 2: Inbound Rejection - No Token

```bash
# run inside test client pod
curl -s -X POST http://git-issue-agent:8080/ \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc": "2.0", "id": "test", "method": "message/send", "params": {"message": {"role": "user", "parts": [{"type": "text", "text": "test"}]}}}' \
  | jq .
```

Expected:
```
{
  "error": "auth.unauthorized",
  "message": "missing Authorization header",
  "plugin": "jwt-validation"
}
```

Exit the test client
```bash
exit
```


### Step 8: Configure Environment Variables and LLM Settings

#### Keycloak Configuration
Edit the `aiac.env` file with your Keycloak connection settings:

```bash
# Keycloak Configuration
KEYCLOAK_URL=http://keycloak.localtest.me:8080
KEYCLOAK_ADMIN_USERNAME=admin
KEYCLOAK_ADMIN_PASSWORD=admin
REALM_NAME=kagenti
```

#### LLM Configuration
- Create `aiac_agent/config/llm_conf.yaml`
- Edit and configure your preferred LLM
- see example [llm_conf](aiac_agent/config/llm_conf.yaml.TEMPLATE) :

### Step 9: Initialize Keycloak Realm
Run the setup script to create the demo realm with clients, roles, and users:

```bash
python setup_keycloak.py config.yaml
```

This creates:

| Resource | Name | Purpose |
|----------|------|---------|
| **Realm** | `kagenti` | Keycloak realm for the demo |
| **Clients** | `demo-ui`, `git-issue-agent`, `github-tool` | Service clients with roles |
| **Realm Roles** | `admin`, `developer`, `sales`, `tech-support` | User roles |
| **Users** | `alice`, `bob`, `charlie` | Demo users with different roles |
| **Client Scopes** | Role-specific scopes with audience mappers | For token exchange |



### Step 10: Initial state - users are not allowed to list issues

⚠️  NOTE ⚠️  - make sure alice|alice123 (bob/bob123) appear in keycloak users under 'kagenti' realm (reset password if needed and make sure temporary password is set)

The user 'alice' is allowed to send requests to the git issue agent, how ever the agent is not allowed to invoke the githb tool.
First, get a valid token from Keycloak, then pass it in the request to the agent endpoint.
Authbridge inbound check will allow the request to proceed to the agent.
The agent in turn, will try to invoke the github tool.
Authbridge outbound check will exchange the token, then deny the request since the exchanged token will not include the github tool in the 'aud' claim.

```bash

REALM_NAME="kagenti"

  ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token \
    -d "grant_type=password" \
    -d "client_id=admin-cli" \
    -d "username=admin" \
    -d "password=admin" | jq -r ".access_token")

SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/${REALM_NAME}/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")
echo "Client ID: $CLIENT_ID  Secret length: ${#CLIENT_SECRET}"



# Initially show that users are not allowed to list issues

ALICE_TOKEN=$(curl -s -X POST \
   "http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token" \
   -d "grant_type=password" \
   -d "username=alice" \
   -d "password=alice123" \
   --data-urlencode "client_id=$CLIENT_ID" \
   --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo $ALICE_TOKEN
echo $ALICE_TOKEN | cut -d. -f2- | base64 -d | jq .


curl -s --max-time 300 \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "alice-public",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-alice-pub-1",
        "parts": [{"type": "text", "text": "List one issue in kagenti/kagenti repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5

# SAME BEHAVIOUR AS FOR ALICE
BOB_TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "username=bob" \
  -d "password=bob123" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Bob token length: ${#BOB_TOKEN}"
echo "Bob scopes: $(echo $BOB_TOKEN | cut -d. -f2 | base64 -d 2>/dev/null | jq -r '.scope')"

echo $BOB_TOKEN
echo $BOB_TOKEN | cut -d. -f2- | base64 -d | jq .

curl -s --max-time 300 \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "bob-public",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-bob-pub-1",
        "parts": [{"type": "text", "text": "List one issue in kagenti/kagenti repoo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5

```

<sub><span style="color: gray; font-size: 0.9em;">
Troubleshooting: \
If INTERNAL_ID shows null, the Keycloak query didn't find the client. \
Verify $ADMIN_TOKEN is not empty (Keycloak reachable?) and that setup_keycloak.py was run.
</span></sub>

<sub><span style="color: gray; font-size: 0.9em;">
You can also list all clients with: \
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients" | jq '.[].clientId'
</span></sub>

<sub><span style="color: gray; font-size: 0.9em;">
if ALICE_TOKEN (or BOB_TOKEN) is null or empty run \
curl -s -X POST "http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token" \
   -d "grant_type=password" \
   -d "username=alice" \
   -d "password=alice123" \
   --data-urlencode "client_id=$CLIENT_ID" \
   --data-urlencode "client_secret=$CLIENT_SECRET" \
Look for error message e.g. "Invalid user credentials" will require updating user credentials in keycloak
</span></sub>


Expected:
```
Failure message, e.g. "I'm sorry I was unable to fulfill your request...."  due to JWT validation failure.

This happends since the git issue agent is not authorized to invoke the github tool yet.
Token exchange will fail (HTTP 400): invalid_client: github tool audience not found
```
#### Check exact failure details in AuthBridge logs

Verify that AuthBridge is handling inbound validation and outbound token exchange:

```bash
# Check AuthBridge/Envoy proxy logs
kubectl logs deployment/git-issue-agent -n team1 -c authbridge-proxy | tail -50
or
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy | tail -50
```

Expected log entries showing:
```
JWT validation failed" error="validating JWT: invalid JWT"
"pipeline: plugin rejected request" plugin=jwt-validation status=401 code=auth.unauthorized reason="token validation failed"
inbound authorized subject="" clientID=spiffe://localtest.me/ns/team1/sa/git-issue-agent
"token exchange failed" host=github-tool-mcp:9090 error="token exchange failed (HTTP 400): invalid_client: Audience not found"
"pipeline: plugin rejected request" plugin=token-exchange status=503 code=upstream.token-exchange-failed reason="token exchange failed"
```



### Step 11: Create a Policy Description

Create a text file with your natural language policy description. Two examples are provided under policies directory e.g. [regular_policy](policies/regular_policy.txt)

### Step 12: Generate and Apply Policy (Full Pipeline)

NOTE - To test policy generation without applying to Keycloak - use the 'generate' flag

Now run the full pipeline to apply the policy to Keycloak:

```bash
python aiac_cli.py policies/regular_policy.txt
```

Review generated files:
  - Configuration: generated_configs/regular_policy_config.yaml
  - Rules: generated_configs/regular_policy_policy.yaml


Verify results
```bash
echo "1. Developer has github-full-access:"
yq '.policy.developer[] | select(.client == "github-tool" and .role == "github-full-access")' generated_configs/regular_policy_policy.yaml

echo -e "\n2. Developer has github-tool-aud:"
yq '.policy.developer[] | select(.client == "github-tool" and .role == "github-tool-aud")' generated_configs/regular_policy_policy.yaml

echo -e "\n3. Tech-support has github-tool-aud:"
yq '.policy.tech-support[] | select(.client == "github-tool" and .role == "github-tool-aud")' generated_configs/regular_policy_policy.yaml

echo -e "\n4. Tech-support does NOT have github-full-access (should be empty):"
yq '.policy.tech-support[] | select(.client == "github-tool" and .role == "github-full-access")' generated_configs/regular_policy_policy.yaml

echo -e "\n5. Sales does NOT exist in policy (should be null):"
yq '.policy.sales' generated_configs/regular_policy_policy.yaml
```
Expected output :
```bash
1. Developer has github-full-access:
client: github-tool
role: github-full-access

2. Developer has github-tool-aud:
client: github-tool
role: github-tool-aud

3. Tech-support has github-tool-aud:
client: github-tool
role: github-tool-aud

4. Tech-support does NOT have github-full-access (should be empty):

5. Sales does NOT exist in policy (should be null):
null
```

### Step 14: Verify Policy in Keycloak

You can verify the applied policy in the Keycloak admin console:

1. Open Keycloak admin console: `http://keycloak.localtest.me:8080/`
2. Login with admin credentials
3. Select the `kagenti` realm
4. Navigate to **Realm roles**
5. Click on a role (e.g., `developer` or `tech-support`)
6. Go to the **Composite roles** tab
7. Verify the client roles are correctly
   e.g. `github-tool-aud`, `github-tool-aud`, `github-full-access` appear in 'Associated roles' for 'Developer' realm role, and no roles appear in 'Associated roles' for 'Sales' realm role.



### Step 15: Test Access Control

Test the policy by getting tokens for different users:

```bash

REALM_NAME="kagenti"

ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/${REALM_NAME}/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")
echo "Client ID: $CLIENT_ID  Secret length: ${#CLIENT_SECRET}"


# step 2 - run AIAC using regualr policy
# python AIAC.py policy/regular_policy.txt
# users will be configured acording to the 'regular' policy
#ALICE (Developer) can list issues in kagenti/kagenti repo
#ALICE can also list issues in omerboehm/intro2c repo (because she is a DEVELOPER and has full access)

ALICE_TOKEN=$(curl -s -X POST \
   "http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token" \
   -d "grant_type=password" \
   -d "username=alice" \
   -d "password=alice123" \
   --data-urlencode "client_id=$CLIENT_ID" \
   --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo $ALICE_TOKEN
echo $ALICE_TOKEN | cut -d. -f2- | base64 -d | jq .


curl -s --max-time 300 \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "alice-public",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-alice-pub-2",
        "parts": [{"type": "text", "text": "List issues in kagenti/kagenti repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5


curl -s --max-time 300 \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "alice-private",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-alice-priv-2",
        "parts": [{"type": "text", "text": "list issues in github.com/omerboehm/intro2c repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5

#BOB (tech-support) can list issues in kagenti/kagenti repo
#BOB can not list issues in omerboehm/intro2c repo (because he is a TechSupport and has public access)

BOB_TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "username=bob" \
  -d "password=bob123" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Bob token length: ${#BOB_TOKEN}"
echo "Bob scopes: $(echo $BOB_TOKEN | cut -d. -f2- | base64 -d 2>/dev/null | jq -r '.scope')"

echo $BOB_TOKEN
echo $BOB_TOKEN | cut -d. -f2- | base64 -d | jq .

curl -s --max-time 300 \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "bob-public",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-bob-pub-2",
        "parts": [{"type": "text", "text": "List one issue in kagenti/kagenti repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5


curl -s --max-time 300 \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "bob-private",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-bob-priv-2",
        "parts": [{"type": "text", "text": "list issues in github.com/omerboehm/intro2c repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5


#Charlie (Sales) still cannot list issues in kagenti/kagenti repo (Role Sales in policy doesnt allow)


CHARLIE_TOKEN=$(curl -s -X POST \
   "http://keycloak-service.keycloak.svc:8080/realms/${REALM_NAME}/protocol/openid-connect/token" \
   -d "grant_type=password" \
   -d "username=charlie" \
   -d "password=charlie123" \
   --data-urlencode "client_id=$CLIENT_ID" \
   --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo $CHARLIE_TOKEN
echo $CHARLIE_TOKEN | cut -d. -f2- | base64 -d | jq .



curl -s --max-time 300 \
  -H "Authorization: Bearer $CHARLIE_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "charlie-public",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-charlie-pub-1",
        "parts": [{"type": "text", "text": "List one issue in kagenti/kagenti repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5



```

### Step 16: Update Policy

To update the policy, simply modify the policy description and re-run:

Create an updated natural language policy description. See example [permissive_policy](policies/permissive_policy.txt)

The AIAC system will:
1. Export the current realm configuration
2. Generate a new policy from the updated description
3. Remove old composite role mappings
4. Apply the new policy

Apply the updated policy
```bash
python aiac_cli.py policies/permissive_policy.txt
```
Review generated files:
  - Configuration: generated_configs/permissive_policy_config.yaml
  - Rules: generated_configs/permissive_policy_policy.yaml


Verify results
```bash
echo "1. Developer has github-full-access:"
yq '.policy.developer[] | select(.client == "github-tool" and .role == "github-full-access")' generated_configs/permissive_policy_policy.yaml

echo -e "\n2. Developer has github-tool-aud:"
yq '.policy.developer[] | select(.client == "github-tool" and .role == "github-tool-aud")' generated_configs/permissive_policy_policy.yaml

echo -e "\n3. Tech-support has github-tool-aud:"
yq '.policy.tech-support[] | select(.client == "github-tool" and .role == "github-tool-aud")' generated_configs/permissive_policy_policy.yaml

echo -e "\n4. Tech-support does NOT have github-full-access (should be empty):"
yq '.policy.tech-support[] | select(.client == "github-tool" and .role == "github-full-access")' generated_configs/permissive_policy_policy.yaml

echo -e "\n5. Sales is now just like Tech-support (\"Other personnel\"):"
yq '.policy.sales[] | select(.client == "github-tool" and .role == "github-tool-aud")' generated_configs/permissive_policy_policy.yaml
```
Expected output :
```bash
1. Developer has github-full-access:
client: github-tool
role: github-full-access

2. Developer has github-tool-aud:
client: github-tool
role: github-tool-aud

3. Tech-support has github-tool-aud:
client: github-tool
role: github-tool-aud

4. Tech-support does NOT have github-full-access (should be empty):

5. Sales is now just like Tech-support ("Other personnel"):
client: github-tool
role: github-tool-aud



### Step 17: Reset Realm (Optional)

To clean up and start fresh:

```bash
# delete generated policies
rm -f generated_configs/*.yaml

# re-provision the realm
python setup_keycloak.py config.yaml
```
