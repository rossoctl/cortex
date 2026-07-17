# Component PRD: RAG Ingest Service

## Description
A FastAPI REST service co-located with ChromaDB in the RAG Pod. Accepts knowledge documents for any configured collection, chunks and embeds them, and writes the resulting vectors into the ChromaDB instance in the same Pod. Supports both access control policies (`aiac-policies`) and org/business domain context (`aiac-domain-knowledge`) through a single collection-parameterized API surface. Developer-driven ingestion is performed via `kubectl port-forward`.

After every successful ingest operation the service publishes a trigger event to the **Event Broker** (NATS JetStream) on the `aiac.apply.policy.build` subject. This causes the AIAC Agent to recompute and apply the updated policy against the live PDP state. All three ingest semantics (replace, update, delete) publish `build`; `rebuild` is an explicit operator-only command issued directly to the Agent and is never triggered by the ingest service.

## Endpoints

The `{collection}` path segment must be a slug from `AIAC_RAG_COLLECTIONS` (default: `policy,domain-knowledge`). Unknown slug → 404.

### Replace — wipe and reload the named collection

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/ingest/{collection}/text` | `{"docs": [{"id": "...", "text": "..."}]}` | Replace entire named collection from a JSON body of text documents |
| POST | `/ingest/{collection}/file` | multipart upload (one or more files) | Replace entire named collection from uploaded files; `doc_id` = filename without extension |
| POST | `/ingest/{collection}/url` | `{"docs": [{"id": "...", "url": "..."}]}` | Replace entire named collection from a JSON body of URLs; service fetches each URL |

**Replace semantics:** drops the ChromaDB collection and recreates it, then ingests all provided documents. Atomic at the collection level — partial failures roll back to an empty collection. An empty `docs` list wipes the collection.

### Update — document-level upsert (additive, never deletes)

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/ingest/{collection}/update/text` | `{"docs": [{"id": "...", "text": "..."}]}` | Upsert documents by `doc_id`; absent `doc_id`s in the collection are left untouched |
| POST | `/ingest/{collection}/update/file` | multipart upload (one or more files) | Upsert documents from uploaded files; `doc_id` = filename without extension |
| POST | `/ingest/{collection}/update/url` | `{"docs": [{"id": "...", "url": "..."}]}` | Upsert documents from URLs; only named `doc_id`s are affected |

**Update semantics:** for each incoming `doc_id`, deletes existing chunks for that `doc_id` then inserts new chunks. All other `doc_id`s in the collection are untouched. An empty `docs` list is a no-op.

### Delete — explicit removal

| Method | Path | Description |
|--------|------|-------------|
| DELETE | `/ingest/{collection}/{doc_id}` | Remove all chunks belonging to `doc_id` from the named collection. `doc_id` not found → 404 |

**Delete** is the only path that removes content from a collection. `/update/*` endpoints never delete as a side effect.

## Post-ingest Event Broker notification

After every successful ingest operation (replace, update, or delete), the service publishes `{"id": ""}` to `aiac.apply.policy.build` on the Event Broker (`NATS_URL`). The publish is non-blocking: ingest success is reported to the caller before the NATS publish completes. Publish failures are logged but do not cause the ingest endpoint to return an error. This preserves ingest availability even when the Event Broker is temporarily unavailable.

The AIAC Agent's durable consumer receives the event and acknowledges it after successful processing. Delivery guarantees (at-least-once, replay on Agent restart) are managed by the Event Broker — the RAG Ingest Service is fire-and-forget from its perspective.

## Collection slug → ChromaDB name mapping

| Slug | ChromaDB Collection Name |
|------|--------------------------|
| `policy` | `aiac-policies` |
| `domain-knowledge` | `aiac-domain-knowledge` |

## Ingest conventions

- Chunking and embedding are applied uniformly across all operations and both collections.
- `doc_id` is stored in ChromaDB chunk metadata on every write to enable document-level update and deletion.
- `/text` and `/url` endpoints take a JSON body `{"docs": [{"id": "...", "text/url": "..."}]}`.
- `/file` endpoints use multipart upload; `doc_id` is derived from the filename (extension stripped). Filename collisions within one call → 400.

## Configuration

| Variable | Default | Source |
|----------|---------|--------|
| `CHROMA_URL` | `http://localhost:8000` | ConfigMap |
| `AIAC_RAG_COLLECTIONS` | `policy,domain-knowledge` | ConfigMap |
| `NATS_URL` | `nats://aiac-event-broker-service:4222` | ConfigMap (`aiac-pdp-config`) |
| `EMBEDDING_BASE_URL` | — | ConfigMap |
| `EMBEDDING_MODEL` | — | ConfigMap |
| `EMBEDDING_API_KEY` | — | Kubernetes Secret |

Adding a third collection is a configuration-only change: add a new slug to `AIAC_RAG_COLLECTIONS` and a corresponding entry in the slug→name map. No code modification required.

## Runtime

- Framework: FastAPI with uvicorn
- Bind: `0.0.0.0:7073`
- Base image: `python:3.12-slim`

## Dependencies (`requirements.txt`)

```
fastapi
uvicorn[standard]
chromadb
httpx
nats-py
```

(Embedding model client TBD — depends on chosen embedding provider)
