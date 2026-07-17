# Event Broker Alternatives: Red Hat AMQ Evaluation (Greenfield, Technology-Neutral)

## Abstract

This document evaluates Red Hat's AMQ messaging product family as a candidate for the AIAC Event
Broker role. The evaluation assumes a greenfield implementation — no part of the AIAC system
exists yet — and uses exclusively functional requirements derived from the PRD's use cases and
architectural principles. All technology-specific constraints (protocol names, client library
names, image identifiers, API call names) have been removed. The question asked is: can this
product satisfy what the system must do?

**Conclusion:** AMQ Broker (Artemis) satisfies all functional requirements and is a technically
sound candidate. Remaining differentiators from NATS JetStream are operational: footprint,
configuration surface, and protocol fit for the AIAC event volume. AMQ Streams retains two
structural incompatibilities that cannot be resolved without changing the PRD's routing design.
AMQ Interconnect is disqualified by the durability requirement.

---

## Functional Requirements

Derived from the PRD's use cases and architectural decisions. All technology-specific language
has been removed — requirements state what the system must do, not how.

| ID | Functional requirement | PRD source |
|----|------------------------|------------|
| FR1 | Events must survive the Agent pod going down and be re-delivered when it restarts — no silent event loss on transient consumer failure | §5 arch decisions, §7.4 |
| FR2 | Each event must be processed by exactly one Agent instance across competing replicas (work-queue semantics) | §7.4, §5 arch decisions |
| FR3 | Failed event processing must be retried a bounded number of times, then moved to a dead-letter destination for operator inspection — a persistently broken event must not block the queue | §7.4, §9 |
| FR4 | The broker must support per-entity event addressing: a new Keycloak client and a new role each produce distinct, independently routable events carrying the entity ID — the consumer must be able to subscribe to a wildcard that covers all entity-scoped events without inspecting message payloads | §7.4, §7.5, §7.8, §5 ("no business logic lives in the consumer") |
| FR5 | The Agent (Python 3.12, FastAPI, asyncio) must be able to consume events asynchronously as a background task; processing must complete before the event is acknowledged | §7.5, §10 |
| FR6 | The RAG Ingest Service (Python 3.12, FastAPI) must be able to publish events to the broker | §7.7 |
| FR7 | The Keycloak SPI (Java) must be able to publish events to the broker | §7.8 |
| FR8 | The broker must be deployable without authentication; Kubernetes ClusterIP network isolation is the intended access control boundary | §7.4, §10 |
| FR9 | The broker must run as a single Kubernetes pod with minimal configuration overhead; no Operator dependency | §8 |
| FR10 | An init container must be able to provision required broker resources idempotently at startup and health-check the broker before the Agent container starts | §5 arch decisions, §9 |

---

## Candidate 1 — AMQ Broker (Apache ActiveMQ Artemis)

**What it is:** A full-featured message broker with persistent storage (NIO journal), native
dead-letter support, and competing consumer semantics via `anycast` routing. Supports AMQP 1.0,
STOMP, MQTT, and OpenWire. Python client: `python-qpid-proton`. Java client: Artemis JMS or
Qpid Proton Java.

### Assessment

| Req | Requirement | Verdict | Detail |
|-----|-------------|---------|--------|
| **FR1** | Events survive Agent pod restart | **Pass** | NIO journal persists messages to disk. Consumer reconnects and pending messages re-deliver automatically |
| **FR2** | Work-queue: one consumer per event | **Pass** | `anycast` routing type delivers each message to exactly one consumer across competing receivers — equivalent to work-queue semantics |
| **FR3** | Bounded retry + dead-letter | **Pass** | Native `dead-letter-address` + `max-delivery-attempts` configuration in `broker.xml`. Max attempts is a simple numeric setting |
| **FR4** | Per-entity dynamic addressing | **Pass** | Artemis supports hierarchical address wildcards (`aiac.apply.#` for multi-level, `*` for single-level). Addresses are auto-created on first publish when auto-create address policy is enabled — a new entity ID at runtime creates a new routable address automatically. A consumer on the wildcard address receives all matching events without payload inspection |
| **FR5** | Python asyncio consumer, ack-after-processing | **Pass** | `python-qpid-proton` provides asyncio consumer with manual message settlement. Ack-after-processing is the standard AMQP disposition model — the consumer accepts or rejects a message after handler completion |
| **FR6** | Python publisher | **Pass** | `python-qpid-proton` sender is straightforward |
| **FR7** | Java publisher (Keycloak SPI) | **Pass** | Artemis JMS client and Qpid Proton Java are both mature and well-supported |
| **FR8** | No-auth deployment | **Pass** | Configurable: `<security-enabled>false</security-enabled>` in `broker.xml`. One XML element; low friction |
| **FR9** | Single pod, no Operator, low footprint | **Partial** | Artemis runs standalone without an Operator. However, a `broker.xml` configuration file must be authored and mounted via ConfigMap — there is no single-flag enablement. RAM footprint is ~256–300 MB. For AIAC's event volume (tens of lifecycle events per day) this is significantly over-provisioned |
| **FR10** | Init container idempotent provisioning | **Pass** | Addresses and queues can be pre-declared in `broker.xml` — on broker start they exist before any consumer connects, making provisioning declarative and idempotent by design. Alternatively, the Artemis management REST API supports idempotent address creation at runtime. Health-checking via HTTP management endpoint |

### Residual concern

**FR9** is the only meaningful remaining differentiator. Artemis is operationally heavier than
warranted for the AIAC use case:

- `broker.xml` must be authored, maintained, and mounted as a ConfigMap in the Event Broker
  deployment manifest
- RAM footprint (~300 MB) is ~15× higher than a comparable lightweight broker for an event bus
  that carries a handful of events per day
- Artemis was designed for enterprise JMS workloads with high message throughput and complex
  routing topologies; the AIAC Event Broker is a simple lifecycle event bus

These are operational trade-offs, not functional failures. The system works correctly with
Artemis — it is simply heavier than necessary.

---

## Candidate 2 — AMQ Streams (Apache Kafka / Strimzi)

**What it is:** A Kafka-based event streaming platform managed by the Strimzi Operator on
Kubernetes. Python client: `aiokafka` or `confluent-kafka-python`. Java client: Kafka producer.

### Assessment

| Req | Requirement | Verdict | Detail |
|-----|-------------|---------|--------|
| **FR1** | Events survive Agent pod restart | **Pass** | Kafka topic retention provides message replay for a configured consumer group |
| **FR2** | Work-queue: one consumer per event | **Partial** | Kafka consumer groups deliver one message per partition. Work-queue semantics emerge only when partition count equals consumer count — it is not a single-setting guarantee and requires careful partition design |
| **FR3** | Bounded retry + dead-letter | **Fail** | Kafka has no native dead-letter queue or `max-delivery-attempts` concept. A DLQ must be implemented as application code: a separate retry topic, a retry consumer service, and a DLQ topic. This is infrastructure the PRD expects the broker to provide natively |
| **FR4** | Per-entity dynamic addressing | **Fail** | Kafka topics are static, pre-declared strings. A runtime-generated entity ID cannot become a new topic without administrator intervention. Per-entity routing collapses to per-type topics (e.g. one topic for all role events), requiring the consumer to inspect message payloads to identify the target entity. The PRD (§5) mandates a consumer that carries no business logic — payload-based routing contradicts this |
| **FR5** | Python asyncio consumer, ack-after-processing | **Pass** | `aiokafka` is a mature asyncio Kafka client; manual commit after processing is idiomatic |
| **FR6** | Python publisher | **Pass** | `aiokafka` or `confluent-kafka-python` |
| **FR7** | Java publisher (Keycloak SPI) | **Pass** | Kafka Java producer is the most mature producer client available |
| **FR8** | No-auth deployment | **Partial** | Kafka supports no-auth, but Strimzi configures mutual TLS by default; explicit opt-out via `KafkaListeners` spec is required |
| **FR9** | Single pod, no Operator | **Fail** | Strimzi Operator is the practical Kubernetes deployment path for Kafka. A single-node KRaft deployment without Operator is possible but not maintainable. Minimum footprint: ~1 GB RAM plus Operator pods |
| **FR10** | Init container idempotent provisioning | **Partial** | Kafka `AdminClient` supports idempotent topic creation. Feasible but requires a Kafka bootstrap connection and more setup than a simple health check |

### Structural incompatibilities

FR3 and FR4 fail regardless of implementation approach:

**FR3 — no native DLQ:** Building bounded retry with dead-lettering requires a separate retry
consumer service, a retry topic, and a DLQ topic. This is application infrastructure, not broker
configuration. The PRD (§7.4) treats dead-lettering as a broker-level property.

**FR4 — static topics vs dynamic addressing:** The PRD's per-entity event addressing is a
first-class routing feature. Collapsing `aiac.apply.role.{id}` to a static topic and
embedding the entity ID in the message payload changes the consumer contract and adds routing
logic to what §5 explicitly defines as a thin adapter with no business logic. This is a
PRD-level design change, not an implementation detail.

---

## Candidate 3 — AMQ Interconnect (Apache Qpid Dispatch Router)

**What it is:** A stateless AMQP 1.0 message router. Routes messages between endpoints with no
on-disk persistence.

### Assessment

| Req | Requirement | Verdict | Detail |
|-----|-------------|---------|--------|
| **FR1** | Events survive Agent pod restart | **Hard fail** | Interconnect is stateless by design. Messages in-flight when the Agent pod restarts are permanently lost |

FR1 is an immediate disqualifier. UC-1 and UC-2 both depend on events surviving transient Agent
failures — this is a first-class architectural decision in §5. AMQ Interconnect can complement
AMQ Broker as a routing mesh but cannot serve as the AIAC Event Broker independently.

---

## Consolidated Scorecard

| Requirement | PRD anchor | NATS JetStream | AMQ Broker | AMQ Streams | AMQ Interconnect |
|-------------|------------|:--------------:|:----------:|:-----------:|:----------------:|
| FR1 Durable replay on pod restart | §5, §7.4 | ✅ | ✅ | ✅ | ❌ |
| FR2 Work-queue / competing consumers | §7.4, §5 | ✅ | ✅ | ⚠️ | ⚠️ |
| FR3 DLQ after bounded retries | §7.4, §9 | ✅ | ✅ | ❌ | ❌ |
| FR4 Per-entity dynamic addressing, wildcard consumer | §7.4, §7.5, §7.8, §5 | ✅ | ✅ | ❌ | ⚠️ |
| FR5 Python asyncio consumer, ack-after-processing | §7.5, §10 | ✅ | ✅ | ✅ | ❌ |
| FR6 Python publisher | §7.7 | ✅ | ✅ | ✅ | ✅ |
| FR7 Java publisher (Keycloak SPI) | §7.8 | ✅ | ✅ | ✅ | ✅ |
| FR8 No-auth viable | §7.4, §10 | ✅ | ✅ | ⚠️ | ✅ |
| FR9 Single pod, no Operator, low footprint | §8 | ✅ | ⚠️ | ❌ | ✅ |
| FR10 Init container idempotent provisioning | §5, §9 | ✅ | ✅ | ⚠️ | ❌ |

✅ Satisfies requirement · ⚠️ Achievable with configuration trade-off · ❌ Cannot satisfy without design change

---

## Comparative Analysis: AMQ Broker vs NATS JetStream

With all technology-specific constraints removed, both candidates satisfy every functional
requirement. The decision reduces to operational fit.

| Dimension | NATS JetStream | AMQ Broker (Artemis) |
|-----------|----------------|----------------------|
| FR1–FR3 delivery semantics | Native, zero-config | Native, `broker.xml` config |
| FR4 per-entity addressing | Hierarchical subjects, auto-routed | Hierarchical addresses, auto-create policy |
| FR5 Python asyncio consumer | Idiomatic, well-documented | Functional, lower-level API |
| FR7 Java publisher | NATS Java client | Artemis JMS / Qpid Proton Java |
| FR8 no-auth | Default, no config needed | One XML element in `broker.xml` |
| FR9 footprint | ~20 MB RAM, single flag | ~300 MB RAM, `broker.xml` ConfigMap |
| FR10 broker provisioning | Single API call, runtime | Declarative via `broker.xml` (no runtime call needed) |
| Protocol lineage | Purpose-built for microservice pub/sub | Enterprise JMS / AMQP 1.0 interoperability |
| Python ecosystem depth | Large community, extensive examples | Smaller community, sparser asyncio docs |
| Design envelope | Lightweight durable pub/sub | High-throughput enterprise queuing |

The functional gap is closed. AMQ Broker is a technically valid choice. The remaining difference
is that it carries more operational weight (configuration, RAM, XML) than the AIAC Event Broker
role requires.

---

## Conclusion

**AMQ Broker (Artemis)** satisfies all ten functional requirements in a greenfield build. It
is a technically sound candidate. The single remaining concern — FR9, operational footprint — is
a trade-off, not a disqualifier: Artemis is ~15× heavier in RAM than warranted for an event bus
that carries tens of lifecycle events per day, and its XML-based configuration adds overhead
absent from simpler brokers.

**AMQ Streams** retains two hard disqualifications regardless of implementation approach: no
native DLQ (FR3) and inability to support dynamic per-entity addressing (FR4). Resolving either
would require changing the PRD's routing design.

**AMQ Interconnect** is disqualified by FR1.

**Selection guidance:**

| Condition | Recommended choice |
|-----------|-------------------|
| No pre-existing AMQ infrastructure; team is starting fresh | Lightweight broker matched to event volume — NATS JetStream |
| Team already operates AMQ Broker for other workloads | AMQ Broker — eliminate a dependency, reuse existing expertise |
| Platform standardised on AMQP 1.0 across services | AMQ Broker — protocol consistency has value |
| Minimising operational surface and configuration files is a priority | NATS JetStream |
| Red Hat support contract covers messaging infrastructure | AMQ Broker |
