# Component PRD: Event Broker

## Description

A NATS JetStream pod that decouples event producers from the AIAC Agent. Producers (Keycloak SPI listener, RAG Ingest Service) publish lightweight trigger events to named NATS subjects. The AIAC Agent subscribes as a durable competing consumer, guaranteeing at-least-once delivery and automatic replay of unprocessed events after pod restarts.

The Event Broker is a single-node NATS JetStream instance. It owns no business logic — it is a pure transport layer. All policy decisions, orchestration, and state remain in the AIAC Agent.

---

## Stream Configuration

| Property | Value |
|---|---|
| Stream name | `aiac-events` |
| Subjects | `aiac.apply.>` |
| Retention policy | `WorkQueuePolicy` — message deleted from stream after acknowledgement |
| Consumer name | `aiac-agent-consumer` |
| Consumer type | Durable push consumer with queue group (competing consumers) |
| Authentication | None — ClusterIP network isolation is the access control mechanism |
| Dead-letter subject | `aiac.apply.dlq` |
| Max delivery attempts | 5 — message routed to DLQ after 5 unacknowledged redeliveries |

---

## Subjects

| Subject | Publisher | Consumer | Trigger |
|---|---|---|---|
| `aiac.apply.service.{id}` | Keycloak SPI listener | AIAC Agent | Keycloak `CLIENT_CREATED` event |
| `aiac.apply.role.{id}` | Keycloak SPI listener | AIAC Agent | Keycloak role created/updated |
| `aiac.apply.policy.build` | RAG Ingest Service | AIAC Agent | Post-ingest completion (any collection) |
| `aiac.apply.dlq` | NATS JetStream (automatic) | Operator (manual inspection) | Max delivery attempts exceeded |

**`rebuild` is not routed through the Event Broker.** It is an operator-only command issued directly via `POST /apply/policy/rebuild` on the AIAC Agent using `kubectl port-forward`.

---

## Message Payload

All messages carry a minimal JSON payload containing only the entity ID:

```json
{ "id": "<entity-id>" }
```

For `aiac.apply.policy.build`, the payload is empty (`{}`). The AIAC Agent pulls all required state from the PDP Configuration Service at processing time — the event payload is a trigger, not a data carrier.

---

## Delivery Guarantees

- **At-least-once delivery** — NATS redelivers any message not acknowledged within the `AckWait` window.
- **Exactly-one processing** — the Agent subscribes via a queue group (`aiac-agent-consumer`). Only one Agent pod receives each message; other pods in the group are not notified.
- **Replay on restart** — `WorkQueuePolicy` retains all unacknowledged messages. A restarted Agent pod automatically receives pending messages on reconnection.
- **DLQ on repeated failure** — after 5 unacknowledged redeliveries, NATS routes the message to `aiac.apply.dlq` for operator inspection. No message is silently dropped.

---

## Configuration

| Variable | Default | Source |
|---|---|---|
| `NATS_URL` | `nats://aiac-event-broker-service:4222` | ConfigMap (`aiac-pdp-config`) |

No authentication credentials are required. The NATS server runs with no-auth configuration.

---

## Runtime

- Image: `nats:latest` with JetStream enabled (`-js` flag)
- Bind: `0.0.0.0:4222` (NATS client port)
- Kubernetes ClusterIP service: `aiac-event-broker-service:4222`
- Base image: official `nats` Docker image

---

## Kubernetes Manifest

`aiac/k8s/event-broker-deployment.yaml` — NATS JetStream Pod Deployment + ClusterIP Service.

---

## AIAC Init Container

A dedicated `aiac-init` init container runs in the **Agent Pod** before the Agent container starts. It orchestrates the AIAC startup sequence:

1. **Wait for NATS** — poll `aiac-event-broker-service:4222` until TCP connection succeeds.
2. **Wait for PDP Configuration Service** — poll `AIAC_PDP_CONFIG_URL/health` until HTTP 200.
3. **Wait for PDP Policy Writer** — poll `AIAC_PDP_POLICY_URL/health` until HTTP 200.
4. **Wait for RAG Ingest Service** — poll `AIAC_RAG_INGEST_URL/health` until HTTP 200 (confirms ChromaDB in the same RAG pod is also up).
5. **Create NATS JetStream stream** — call `js.add_stream()` idempotently with the `aiac-events` stream configuration. Safe to call on every restart.

The init container uses `python:3.12-slim` with `nats-py` and `httpx`. It is version-controlled alongside the Agent. All dependency URLs are read from the `aiac-pdp-config` ConfigMap.

### Init Container Configuration

| Variable | Source | Resolves to |
|---|---|---|
| `NATS_URL` | ConfigMap (`aiac-pdp-config`) | `nats://aiac-event-broker-service:4222` |
| `AIAC_PDP_CONFIG_URL` | ConfigMap (`aiac-pdp-config`) | `http://aiac-pdp-config-service:7071` |
| `AIAC_PDP_POLICY_URL` | ConfigMap (`aiac-pdp-config`) | `http://aiac-pdp-policy-service:7072` |
| `AIAC_RAG_INGEST_URL` | ConfigMap (`aiac-pdp-config`) | `http://aiac-rag-service:7073` |

### Init Container Dependencies (`requirements.txt`)

```
nats-py
httpx
```

---

## Testing

| Target | What to mock | What to assert |
|---|---|---|
| Init container health-check loop | HTTP 4xx then 200 sequence | Exits 0 only after all four dependencies respond healthy |
| Init container stream creation | NATS JetStream `add_stream` call | Called with correct stream name, subjects, and retention policy; idempotent on second call |
| Agent NATS consumer dispatch | NATS message delivery | Correct `/apply/*` handler invoked for each subject pattern; message acked on success; message not acked on handler exception |
| DLQ routing | NATS max redelivery exceeded | Message appears on `aiac.apply.dlq` after 5 failures |
