# Plan: ReasonR integrations — the bus, external systems, fakes

| | |
|---|---|
| **Basis** | [RFC-0001](https://github.com/tycheg/reasonr-compiler/blob/main/docs/rfc/0001-reasonr-language/README.md), stage 5 of the implementation plan (§38): "Kafka, the integration service", criterion — the end-to-end taxi scenario with stubs; RFC §13 (notifications), §15 (timers), §18 (external systems), §27 (scenario tests), §31 (the bus, the integration service); stage E4 of the engine plan ([0003-engine.md](0003-engine.md)); decision 11 of the personal data plan ([0004-personal-data.md](0004-personal-data.md)) |
| **Status** | Approved 2026-10-05 with the decisions of [§2](#2-decisions-to-approve); a revision of decisions 1, 2, 13 and the new decisions 17–22 — 2026-10-06 ([§2.1](#21-revision-2026-10-06)). Done; progress — [0005-integrations-progress.md](0005-integrations-progress.md) |
| **Date** | 2026-10-05 |
| **Implementation language** | Go 1.23+; the first bus adapter is Kafka on `github.com/twmb/franz-go` in a separate module `adapters/kafka`; JWT — `edge/auth` (verification exists, signing is added) |
| **Note** | Paths, package names and RFC section numbers are those of the time (the repository `tycheg/reasonr` before the split) |

---

## Contents

1. [Goal and scope](#1-goal-and-scope)
2. [Decisions to approve](#2-decisions-to-approve)
   - [2.1. Revision 2026-10-06](#21-revision-2026-10-06)
3. [The language: the system's role, the request timeout, fakes](#3-the-language-the-systems-role-the-request-timeout-fakes)
4. [Delivering external requests](#4-delivering-external-requests)
5. [The integration service contract](#5-the-integration-service-contract)
6. [The bus](#6-the-bus)
7. [Fakes in tests and engines](#7-fakes-in-tests-and-engines)
8. [The end-to-end taxi scenario](#8-the-end-to-end-taxi-scenario)
9. [Configuration and checks](#9-configuration-and-checks)
10. [Stages I1–I7](#10-stages-i1i7)
11. [Testing](#11-testing)
12. [Performance](#12-performance)
13. [Schedule and milestones](#13-schedule-and-milestones)
14. [Risks](#14-risks)
15. [Acceptance criteria](#15-acceptance-criteria)
16. [What changes in the RFC](#16-what-changes-in-the-rfc)

---

## 1. Goal and scope

**Goal:**

- external systems are described in the model completely: the request, the role the system answers with, the timeout as a model timer, and a fake implementation in ReasonR for tests;
- the engine delivers requests in parallel and independently of each other by an open contract with JWT;
- events and, if configured, notifications are published to a bus — Kafka or another — in one format.

**What already exists** (stages 3–4):

| Part | Where | State |
|---|---|---|
| The effect stream: outbox, cursors, leases, retries, dead letters, `reasonrd dispatch retry` | `engine/dispatch` | Done |
| The HTTP request transport: `POST {url}/requests/{System}/{request}`, `Idempotency-Key`, `X-Reasonr-Tenant`, `X-Reasonr-Timeout`, a static `Bearer` token | `edge/integration/http.go` | Changes: JWT, a deadline instead of a duration (§5) |
| Requests are delivered by **one stream** `requests` by position: an unanswered request holds every following one until its `timeout` | `dispatch.Requests` | Replaced by a delivery queue (§4) |
| `request … timeout 30s` — a delivery deadline separate from the model's timer | the compiler, `reasonr.ExternalRequest.Timeout` | Replaced by a timer parameter (§3.2) |
| The `Publisher` port, `bus.publish` — a list of events, the `log` bus; personal arguments are none | `engine/port/effects.go`, `dispatch.Publish`, `engine/builtin` | Changes: every event is published, notifications if configured, CloudEvents |
| Notifications do not get into the outbox: to the initiator — in the response, to others — through the hub | `engine/kernel`, `port.NotificationHub` | They do if their publishing is enabled (§6) |
| JWT verification (`auth: jwt`: a secret, a key, JWKS; tenant, actor, role claims) | `edge/auth/jwt.go` | Done; a tenant from a header for a system token is added (§5.3) |
| Scenario tests: external requests are recorded, the test calls the systems' responses itself (`as matching`) | `testrun`, `engine/testapi` | Extended with fakes (§7) |

**In the plan:**

- the language: an external system's role, a `timeout Timer.Id` parameter of a request, `fake` implementations in `*.fake.rr` files;
- a delivery queue for external requests: each request is delivered and retried independently, in parallel; delivery stops when the timeout timer fired or was deleted;
- the integration service contract: HTTP, OpenAPI from the model, JWT in both directions;
- the bus: the CloudEvents format, publishing every event and, if configured, notifications; the conformance suite `RunPublisher`; the `memory` and Kafka adapters;
- fakes in `reasonr test` (memory and HTTP) and `reasonrd --test`;
- the end-to-end taxi scenario on fakes, a contract and Kafka check in a Go test, documentation, measurements.

**Not in the plan:**

- a Go SDK and ready integration services: a service is written in any language by the contract of §5;
- a second bus adapter (NATS, RabbitMQ) — the E6 backlog; the format and the conformance suite are shared, an adapter is added without changing the engine;
- Kafka as a request transport and as command ingress — backlog;
- a gRPC transport to the integration service — the E6 backlog;
- Avro, Protobuf, Schema Registry, AsyncAPI — backlog;
- own fakes in tenant extensions — backlog; the base model's fakes work in tenant tests too.

---

## 2. Decisions to approve

| # | Question | Proposed decision | Alternative |
|---|---|---|---|
| 1 | Delivering external requests | **A delivery queue**, not a stream by position: an outbox consumer moves requests into a queue in the main store (`port.RequestQueue`, like timers) and moves its cursor right away. A pool of workers (`integrations.workers`, 64 by default) leases ready requests of every tenant and system and sends them in parallel; each request has its own attempts and pause (1, 2, 4… s, up to a minute). An unavailable system delays only its own requests | Sending a batch from the outbox in parallel with a cursor over the prefix — an unsent request still holds the cursor, and with it the retries of every following one |
| 2 | The request timeout | **An optional parameter `timeout Timer.Id`** of a `request`; the construct `timeout 30s` is removed. The model schedules a timer with `schedule` and passes it to the request: `Matching.findDriver(…, timeout: timer)`. The delivery deadline is the timer's time. When the timer fired (the model reacts with its command) or was deleted (a response came, the order was canceled), the engine stops delivering the request. A request without `timeout` is retried until `limits.request_ttl` (24 h), then becomes a dead letter | Keep `timeout 30s` next to the model's timer — two deadlines for the same thing, easy to get out of sync (taxi now has 30 s and 60 s) |
| 3 | The role of an external system | **In the declaration:** `external(matching) Matching`. The fake needs the role (it calls commands on its behalf), as do the check "response commands are available to the system's role" and the system's JWT. The role is optional; without it the system has no fake | A role in the fake header `fake(matching) Matching` — a model without fakes would not know who answers |
| 4 | The integration service contract | **HTTP + JSON, described by an OpenAPI from the model:** `reasonr build --integrations-openapi` and `GET /openapi/integrations.json` — an operation per request with a JSON Schema of the arguments. The system's responses are public commands of the model API (`/openapi.json`). A service is written in any language, there is no SDK | A Go SDK — integration services are usually not written in Go |
| 5 | Authentication engine → service | **A JWT signed by the engine** (ES256): `iss` — the engine name, `aud` — the system name, `tenant`, `jti` — the idempotency key, `exp` — 5 min. Public keys — the engine's `GET /.well-known/jwks.json`, rotation through `kid`. The service needs no shared secret | OAuth2 client credentials at the company's IdP (the engine gets a token) — a dependency on the IdP, in tests too; can be added as a second type later |
| 6 | Authentication service → engine | **A JWT from the IdP** through the existing `auth: jwt`: the system's role is in the roles claim. A service that serves every tenant gets a token with the tenant `*`; then the tenant is taken from the `X-Reasonr-Tenant` header (the service returns it from the request). Only a token all of whose roles are roles of external systems may do this (§3.1) | A token per tenant — the service keeps a table "tenant → credentials", and every new tenant changes its configuration |
| 7 | What a fake is | **A type `fake <System>`** in `*.fake.rr` files: one instance per tenant, without `id`, built like an entity — fields, events, commands (public ones for tests, private ones for itself), `schedule` timers, implementations of the system's requests. Fields change only through events, as in an entity (E0301): one set of rules for the model and fakes | Assigning fields right in the fake's commands — shorter, but a second set of language rules |
| 8 | What a fake sees and does | The request arguments, its own fields, `now`; it calls its own commands and **the model's public commands on behalf of the system's role** (the compiler checks access). It does not read the model's entities, projections and queries: an external system sees only the API | Reading the model (`[orderId]`) — a fake that knows more than the real system hides bugs in the model |
| 9 | A fake before initialization | **Does not exist** (there were no events), like an entity: requests to the system are only recorded, as now. A public command of the fake from a test (`Matching.Setup(…)`) creates it. Existing tests do not change | A fake always active with default values — every test that calls `DriverFound as matching` itself would get extra responses |
| 10 | Where fakes are compiled | **Only into the test program:** `reasonr test`, `reasonrd --test`, `reasonr check` (the check). `reasonr build` and `reasonrd` without `--test` do not include them; the model cannot refer to a fake | Fakes in `*.test.rr` — `reasonrd --test` would not see them for manual checks, and several test files share one set of fakes |
| 11 | How a fake runs | A request → a unit of work of the fake after the commit of the unit that sent it. Calls of the model's public commands from the fake → separate external calls with the system's role, in order, after the commit of the fake's unit. In a test the cascade finishes inside the step: `then` sees the notifications and requests of the cascade ("contains"), errors are compared only for the command of the `when` step. An error of a command the fake called fails the test (kind `fake`), like an error of a `given` step. The cascade is bounded (1000 units per step) — otherwise a `fault` | Only recording the errors of fake calls — a silent logic failure the test would not notice |
| 12 | The bus format | **CloudEvents 1.0, binary mode.** CloudEvents has bindings for Kafka, NATS, AMQP, MQTT, so the format is one for any bus. The value is the arguments in the canonical JSON (RFC §31.3); `ce_id` = `{tenant}/{position}/{number}` (for a notification — `n{number}`, progress, decision 39), `ce_type` = `Order.paymentReceived`, `ce_source` = `reasonr/{tenant}/{Type}`, `ce_subject` = the id of the entity or of the recipient, `ce_time`; the extensions `tenant`, `position`, `kind` (`event`, `notification`) | Our own envelope — consumers' standard tools and SDKs do not understand it |
| 13 | What is published | **Every event**, if the bus is enabled: `bus.publish` is removed, the C0006 check is dropped. **Notifications** — if `bus.notifications: true`, all of them: addressed ones and those to the initiator (the recipient is the initiating actor; a call by a role has no recipient). Personal arguments are none (decision 11 of plan 0004). Notifications get into the outbox only when publishing is enabled — without it the command path does not change | A list of events in the configuration — every new event must not be forgotten |
| 14 | Channels and order | The adapter gets a **logical channel** `{kind}.{Type}` (`event.Order`, `notification.Customer`) and a key: the entity id for an event, the recipient id for a notification. The topic name (Kafka) or subject (NATS) is the adapter's template, by default `reasonr.{kind}.{type}`. Order — within a key; delivery — at least once, duplicates are filtered by `ce_id` | One channel for everything — the consumer has to filter |
| 15 | The Kafka client | **franz-go:** pure Go, an idempotent producer, `acks=all`, a key partitioner compatible with Java (murmur2), TLS and SASL. A separate module — the dependency does not get into the kernel | `segmentio/kafka-go` — a weaker idempotent producer; `confluent-kafka-go` — cgo |
| 16 | The end-to-end scenario | **The taxi scenario tests on fakes** (`examples/taxi/ride.test.rr`): an order, matching, a ride, payment, failures — without manual system responses; they pass `reasonr test --engine memory` and `--engine http` (memory, PostgreSQL, MongoDB). A Go test checks the contract of §5 and Kafka: the engine, Kafka in Docker, a service on `httptest` by the contract with JWT in both directions | A separate build with YAML stubs — a second language for describing system behavior |

### 2.1. Revision 2026-10-06

After I3 decisions 1, 2 and 13 were revised: a delivery queue with dead requests and `request_ttl` is more complex than needed, and not every outbox consumer needs order. Decisions 17–22 are new.

| # | Question | Decision |
|---|---|---|
| 1 | Delivering external requests | **Requests are written to `rt_requests` in the same transaction as the command**, not through the outbox: there is no `requests` consumer and no cursor. The sender takes ready requests of every tenant and system, up to `integrations.workers` at a time, and sends them in parallel. During an attempt the record is claimed — two engine instances do not send one request at the same time. Success — the record is deleted; an error — the record holds the time of the next attempt (§4). The order of requests is not kept |
| 2 | The request timeout | The parameter `timeout Timer.Id` stays. The timer is checked before every attempt: if it fired or was deleted, the retries stop and the record is deleted (the model reacts to the unanswered request). **A request without a timer is "fire and forget":** retries by the strategy until `max_elapsed` is used up, then the record is deleted (a log line, a metric). There are no dead requests, no `limits.request_ttl` and no `reasonrd requests retry` |
| 13 | Notifications to the bus | **A separate channel without delivery guarantees:** with `bus.notifications: true` notifications are published after commit, like into the hub — not through the outbox and not through the event store. Lost on a failure — not retried |
| 17 | The retry strategy | **One for every request:** `integrations.retry: { initial: 1s, multiplier: 2, max_interval: 1m, max_elapsed: 1h }`. `max_elapsed` — only for requests without a timer: the timer sets the deadline of a request with one |
| 18 | The transport to the integration service | **The abstraction `port.IntegrationTransport`; the engine's job is to get the request out.** `http`: the service accepts the request (2xx) and runs it itself, asynchronously. `kafka`: the engine writes the request to a topic, the service reads it and retries itself; the engine retries only a failed write. The `kafka` transport — in I4, together with the Kafka bus module |
| 19 | The event store | **The "event journal" is renamed the "event store"**, so as not to confuse it with a WAL: the history of all of the model's events (`events:` in `engine.yaml`, the table `ev_events`) |
| 20 | Where the bus takes events from | **From the event store**, in position order, with its own cursor: the store is required if the bus is enabled (checked by `reasonr engine check`). The guarantee for events — at least once, ordered within an entity |
| 21 | Writing a projection | **A projection row is written whole**, not only the fields passed (RFC §16, item 4 changes): the compiler requires every field except those with a default value. A row has a version: the position of the unit of work. A write applies only if its version is greater than the stored one, so asynchronous projections apply out of order and a repeat is safe |
| 22 | Deleting a projection row | **A deletion mark with a version:** a write with a smaller version does not resurrect the row. Physical deletion — an open question (progress) |

---

## 3. The language: the system's role, the request timeout, fakes

### 3.1. The role of an external system

```rr
role matching
role payments

external(matching) Matching
  request findDriver(orderId Order.Id, pickup GeoPoint, requestedClass CarClass, exclude set<Driver.Id>, timeout Timer.Id)

external(payments) Payments
  request charge(paymentId Order.Id, amount Money)
```

- The role is declared with `role`; one role may have several systems.
- A new compiler warning: a system with a role has no public command available to that role — it has nothing to answer with.

### 3.2. The timeout is a model timer

```rr
  command findDriver()
    let timer = schedule(at: now + 60s, run: searchTimedOut())
    Matching.findDriver(orderId: id, pickup: route.from, requestedClass: carClass, exclude: declinedBy, timeout: timer)
    searchStarted(timer: timer)
```

- A parameter named `timeout` of a `request` is special:
  - its type is `Timer.Id` or `Timer.Id?`; `none` means a request without a deadline;
  - it is not part of the request body: the engine passes the deadline in a header (§5.2).
- The timer is an ordinary timer of the sending entity: if the model did not delete it, it fires and calls its command. There is no separate "timeout command".
- The engine stops delivery when the timer is gone: it fired or was deleted. A request never delivered is not a dead letter (the model has already reacted) but the metric `reasonr_requests_total{outcome="expired"}`.
- New compiler errors:
  - a `timeout` parameter of another type;
  - the old form `request … timeout 30s` — with a fix hint: schedule a timer and pass it as an argument.
- `reasonr.ExternalRequest.Timeout` (a duration) is replaced by `Timer` (a `Timer.Id`).
- In a test's `then` the `timeout` argument may be omitted, like any other.

### 3.3. A fake

```rr
# taxi/matching.fake.rr

fake Matching
  drivers list<Driver.Id>
  silent bool

  ## Drivers the matching service offers, in order
  command Setup(freeDrivers list<Driver.Id>)
    configured(freeDrivers: freeDrivers)

  ## The matching service stops answering
  command GoSilent()
    silenced()

  event configured(freeDrivers list<Driver.Id>)
    drivers = freeDrivers

  event silenced()
    silent = true

  request findDriver(orderId Order.Id, pickup GeoPoint, requestedClass CarClass, exclude set<Driver.Id>)
    if silent
      return
    for d in drivers
      if d not in exclude
        [orderId].DriverFound(driverId: d)       # a call of the model API on behalf of the role matching
        return
    [orderId].NoDriverFound()
```

```rr
# taxi/payments.fake.rr

fake Payments
  limit decimal

  command Setup(limitAmount decimal)
    configured(limitAmount: limitAmount)

  event configured(limitAmount decimal)
    limit = limitAmount

  request charge(paymentId Order.Id, amount Money)
    if amount.amount > limit
      [paymentId].PaymentDeclined(reason: "limit exceeded")
      return
    schedule(at: now + 5s, run: capture(paymentId: paymentId))   # the response is not immediate

  command capture(paymentId Order.Id)
    [paymentId].PaymentCaptured(transactionId: "tx-" + string(paymentId))
```

Rules:

1. `fake X` is declared only for an `external X` with a role; at most one fake per system.
2. A fake's `request` repeats the parameters of the system's request (without `timeout`); the body is like a fake command's. A request without an implementation in the fake is only recorded.
3. What is available in a fake:
   - allowed: its own fields, events, commands, `schedule`, `now`;
   - not allowed: accessing the model's entities and projections, the model's queries, `forget`, notifications, external requests.
4. `[x].Command(…)` in a fake is an external call of a public command of the model on behalf of the system's role; the compiler checks access the same way as for the API.
5. A fake's public commands are called only from tests, in `given` or `when`, without `as`: `Matching.Setup(freeDrivers: [d1])`.
6. The model cannot refer to a fake, and tests address it only this way (item 5); `reasonr build` does not include it (decision 10).
7. Fakes support `reasonr fmt`, the LSP, `reasonr explain Matching` and `reasonr graph` (an arrow "the fake answers with a command").

---

## 4. Delivering external requests

> Revised 2026-10-06 (§2.1, decisions 1, 2, 17, 18); the I2 delivery queue is replaced.

```yaml
integrations:
  workers: 64
  retry: { initial: 1s, multiplier: 2, max_interval: 1m, max_elapsed: 1h }
  default: { type: http, url: http://integrations:8090 }
```

- **Writing on commit.** The external requests of a unit of work are written to `rt_requests` of the main store in the same transaction: the tenant, the idempotency key, the system and the request, the arguments (personal ones as ciphertext), the position and the time of the unit, the `timeout` timer, the time of the next attempt, the attempt number. Requests do not go through the outbox.
- **Sending.** The sender (the `dispatch` role) claims ready records of every tenant — `for update skip locked` in PostgreSQL, a conditional update in MongoDB — up to the number of free sends of `integrations.workers` and sends them in parallel. The claim is the time of the next attempt, moved by the attempt's duration: if the instance died, the record becomes ready again.
- **Before an attempt** the `timeout` timer is checked with one query to the store; the arguments are decrypted (a forgotten subject — the rule of decision 11 of plan 0004).
- **Attempt outcomes:**

  | Outcome | What happens to the record | Metric `outcome` |
  |---|---|---|
  | 2xx | deleted | `delivered` |
  | the `timeout` timer is gone: it fired or was deleted | deleted without an attempt | `expired` |
  | the subject of a non-optional personal argument is forgotten | deleted without an attempt | `forgotten` |
  | an error, the strategy is not used up | the time of the next attempt by `integrations.retry` | `retry` |
  | an error, a request without a timer, `max_elapsed` since the unit's time is used up | deleted, a warning in the log | `dropped` |

  A refusal by the recipient (4xx) is retried like a failure: the service, not the engine, decides that a retry is useless.
- **Implementations:** memory, PostgreSQL, MongoDB. The conformance suite `RunRequests` checks:
  - writing on commit: the request is visible right away, with the position and time of its unit of work;
  - parallel claiming without handing a record out twice;
  - a retry at its time;
  - an expired claim (an instance restart);
  - the timer check.
- **Removed from I2:** `port.RequestQueue.EnqueueRequests`, the outbox consumer `requests`, dead requests, `limits.request_ttl`, `reasonrd requests retry`. Requests stay in outbox records for now: the test API (`captured`) and the fake cascade see a step's requests through them; they can be removed from there once the test API learns to read `rt_requests`.

---

## 5. The integration service contract

### 5.1. The exchange

| Direction | Request | Response |
|---|---|---|
| Engine → service | `POST {url}/requests/{System}/{request}`, the body is the arguments in the canonical JSON (without `timeout`) | 2xx — accepted; 4xx except 408 and 429 — a permanent refusal (a dead letter); anything else — a retry |
| Service → engine | A public command of the model API: `POST /api/{Type}/{id}/{Command}` with `Idempotency-Key` | 200 — accepted; 422, 403 — errors of the response command (RFC §18, item 4) |

- The service decides itself whether to answer synchronously or later: the engine waits only for a 2xx.
- Retries come with the same `Idempotency-Key`. It is better to answer a retry with a command with the same key — then the engine does not apply the response twice.

### 5.2. The headers of an engine request

| Header | Value |
|---|---|
| `Authorization` | `Bearer <the engine's JWT>` (§5.3) |
| `Idempotency-Key` | The request key: the same for every attempt |
| `X-Reasonr-Tenant` | The data tenant |
| `X-Reasonr-Deadline` | The time of the `timeout` timer (RFC 3339); no header — the request has no deadline |
| `X-Reasonr-Attempt` | The attempt number, from 1 |

### 5.3. JWT

- **Engine → service** (decision 5):
  - an ES256 signing key in PEM — `integrations.auth: { key_file | key_env, kid }`;
  - public keys — `GET /.well-known/jwks.json` on the engine's incoming HTTP;
  - claims: `iss`, `aud` (the system), `tenant`, `jti` (the idempotency key), `iat`, `exp`;
  - in `--test` the key is created at start.
- **Service → engine** (decision 6): `auth: jwt` as now, plus `tenant: { claim: tid, any: "*" }`. The value `*` allows the `X-Reasonr-Tenant` header if every role of the token is a role of an external system of the models; otherwise 403.
- The contract's OpenAPI (decision 4) describes both sides:
  - the request operations with a JSON Schema of the arguments;
  - the headers and the security scheme;
  - references to the response commands available to the system's role.

---

## 6. The bus

```yaml
bus:
  type: kafka
  brokers: [kafka:9092]
  topic: "reasonr.{kind}.{type}"     # {tenant}, {kind}: event | notification, {type}
  notifications: true                # publish notifications without guarantees (decision 13, §2.1)
  tls: { ca_file: /etc/kafka/ca.pem }
  sasl: { mechanism: scram-sha-512, user: reasonr, password_env: KAFKA_PASSWORD }
```

- **`port.Message`** becomes shared by events and notifications: the kind, the logical channel, the key, `Index` (the number in the unit), `At`, the arguments.
- **A shared package `engine/bus`** encodes CloudEvents (decision 12): the adapter gets ready headers and a body and only has to send them.
- **Events come from the event store** (decisions 19, 20): the bus reads it in position order with its own cursor, the guarantee is at least once.
- **Notifications — after commit, without guarantees** (decision 13 as revised in §2.1): with `bus.notifications: true` they are published where the hub gets them — into their own channel `notification.{Type}`; personal arguments are none. They are not written to the outbox or the event store.
- **Adapters:** `memory` (reading in tests, the reference), `log`, `kafka`.
- **The conformance suite `RunPublisher`** checks:
  - every event of a unit is published; notifications — without guarantees;
  - order within a key;
  - personal arguments are none;
  - `ce_id` is unique and does not change on a retry.
- **Kafka:**
  - `ProduceSync` of a batch; the cursor moves after every record is acknowledged;
  - an authorization error and a missing topic are `port.ErrPermanent`;
  - at start the engine checks that the topics exist; `reasonrd migrate` creates them only with `create_topics`, `--test` always; the list — `reasonrd topics` (progress, decision 55);
  - the metrics `reasonr_bus_published_total{channel}`, `reasonr_bus_publish_seconds`.
- **The `kafka` request transport:** the topic `reasonr.request.{system}`, the CloudEvents format (`ce_id` is the idempotency key), any write error — a retry (progress, decision 44).

---

## 7. Fakes in tests and engines

| Where | How a fake runs |
|---|---|
| `reasonr test --engine memory` | The test runner's in-memory engine: after the commit of each unit of a step its requests to initialized fakes run right away, as a cascade (decision 11) |
| `reasonr test --engine http`, `reasonrd --test` | The `fake` transport in the engine. The requests of an ephemeral tenant are run by fakes synchronously at the end of every test API call — the same way as timers when the clock moves; a step returns the result of the whole cascade. Fake commands — `POST /_test/{tenant}/fakes/{System}/{Command}` |
| `reasonrd --test` by hand | `integrations: { default: { type: fake } }`: the requests of ordinary tenants go to the fakes through the delivery queue; a fake is set up with the same `/_test/…/fakes` |

- A fake's state is stored as an entity row of the test program.
- `reasonr test -v` shows the steps of the cascade: the request, the fake's unit, the calls of model commands and their outcomes.

---

## 8. The end-to-end taxi scenario

`examples/taxi/ride.test.rr` (the fakes `matching.fake.rr`, `payments.fake.rr` of §3.3):

| # | Test | What it checks |
|---|---|---|
| 1 | An order → the matching fake offers a driver → the driver accepts and finishes the ride → after 5 s the payments fake charges the money | The status `paid`, the notifications `driverOnTheWay` and `receipt` to the customer; not a single manual system response |
| 2 | The amount is above the payments fake's limit | The status `unpaid`, the notification `paymentProblem` |
| 3 | There are no free drivers | `NoDriverFound` → `noDrivers` |
| 4 | The first driver is busy | The fake gets `exclude` with them and offers the second one |
| 5 | The matching fake is silent (`GoSilent`) | The `timeout` timer restarts the search, after three attempts — `noDrivers` |
| 6 | The customer cancels the order while the matching fake is silent | The search timer is deleted: `searchTimedOut` does not fire, there are no new requests |

The tests pass `reasonr test --engine memory` and `--engine http` on memory, PostgreSQL and MongoDB with the same report.

**A Go test** `cmd/reasonrd/server/integrations_test.go` (PostgreSQL, Kafka from `REASONR_TEST_KAFKA`):

- a service on `httptest` by the contract of §5 verifies the engine's JWT by JWKS;
- the service answers with commands with a system JWT with the tenant `*` into two tenants;
- one system answers 503 — the requests of another are delivered without delay;
- the engine restarts in the middle of the scenario;
- in Kafka — the order's events in order and the notifications, personal arguments are none.

---

## 9. Configuration and checks

```yaml
integrations:
  workers: 64
  retry: { initial: 1s, multiplier: 2, max_interval: 1m, max_elapsed: 1h }
  auth: { key_env: REASONR_INTEGRATIONS_KEY, kid: "2026-10" }   # the engine's JWT signature (ES256, PEM)
  default:  { type: http, url: http://integrations:8090, timeout: 10s }
  Payments: { type: http, url: http://payments-gw:8090 }
auth:
  - type: jwt
    jwks: https://idp/.well-known/jwks.json
    tenant: { claim: tid, any: "*" }
    roles:  { claim: roles }
bus: { type: kafka, brokers: [kafka:9092], notifications: true }
events: main                     # the event store: required with bus
```

`reasonr engine check`:

- **C0006** is dropped together with `bus.publish`; `bus.publish` in the configuration is an error with a hint;
- **C0010** (an error) — the `fake` transport without `--test`;
- **C0011** (a warning) — the JWT has `tenant.any` set, but the external systems of the models have no roles: there is nobody to issue an all-tenants token to;
- **C0012** (an error) — `integrations.auth` is not set while systems have an HTTP transport (except with `--test`);
- **C0013** (an error) — the bus is enabled but there is no event store (`events`).

---

## 10. Stages I1–I7

### I1. The language and tests in memory

- The compiler: `external(role)`, the `timeout` parameter, `fake` in `*.fake.rr` — the parser, semantics, diagnostics, `fmt`, LSP, `explain`, `graph`.
- The VM: an external command call from a fake.
- The in-memory test runner: the fake cascade, the `-v` report, the failure kind `fake`.
- Taxi: a timer in `findDriver`, the fakes and `ride.test.rr` of §8.
- **Done when:** negative tests for every new diagnostic; the taxi tests (the earlier ones and `ride.test.rr`) pass in memory.

### I2. Request delivery

- The first version — a delivery queue through the outbox with dead requests — was done and is being replaced (§2.1).
- The rework (§4): `rt_requests` is written on commit; parallel sending with claiming; `integrations.retry`; stopping by the timer; no dead requests and no `request_ttl`; `RunRequests` on memory, PostgreSQL, MongoDB.
- **Done when:** `RunRequests` passes on three stores; an unavailable system does not delay another; a deleted timer stops the retries; a request without a timer is dropped after `max_elapsed`; the contract Go test (I3) passes.

### I3. The contract and JWT

- The engine's JWT and JWKS; the headers of §5.2; `tenant.any` in `auth: jwt`; `reasonr build --integrations-openapi`, `GET /openapi/integrations.json`; C0010–C0012.
- **Done when:** the contract Go test (§8) without Kafka passes on PostgreSQL; the OpenAPI passes schema validation.

### I4. The bus

- `engine/bus` (CloudEvents), publishing every event from the event store (C0013), notifications without guarantees, the `memory` adapter, `RunPublisher`.
- The module `adapters/kafka`: the bus and the `kafka` request transport (decision 18); a test in Docker (`REASONR_TEST_KAFKA`, the Apache Kafka KRaft image).
- **Done when:** `RunPublisher` passes on `memory` and Kafka; with the broker unavailable an entity's messages are neither lost nor reordered.

### I5. Fakes over HTTP

- The `fake` transport, the cascade in the test API, `/_test/{tenant}/fakes`, `reasonr test --engine http` with fakes.
- **Done when:** the taxi tests pass over HTTP (memory, PostgreSQL, MongoDB) with the same report as in memory; the full Go test of §8 with Kafka passes.

### I7. Projections by version

- The language: a projection is written whole — the compiler requires every field except those with a default value (a new diagnostic); RFC §16, item 4; taxi — the `OrderMart` row is written by one command with every field.
- The engine: the row version is the position of the unit of work; a write applies only if its version is greater than the stored one; deletion is a mark with a version (decisions 21, 22); asynchronous projections apply out of order, a repeat is safe. Memory, PostgreSQL, MongoDB; the projection conformance suite.
- **Done when:** the projection conformance suite passes with a shuffled apply order and repeats on three stores; the taxi tests pass.

### I6. Load and documentation

- The measurement of §12.
- The RFC (§16), the README "Integrations", `AGENTS.md` (fakes, `timeout`, checking scenarios), `engine.yaml` examples.
- **Done when:** the criteria of [§15](#15-acceptance-criteria) are met.

---

## 11. Testing

| Kind | What it checks | How |
|---|---|---|
| The compiler | `external(role)`, `timeout`, fakes, access of fake calls, the bans of §3.3 | Negative tests for every diagnostic, IR and bytecode golden files |
| Scenarios | The taxi logic with fakes | `ride.test.rr` in memory and over HTTP (three stores) |
| Conformance | The delivery queue, the bus | `RunRequests` on three stores, `RunPublisher` on `memory` and Kafka |
| The contract | Headers, JWT of both sides, the tenant from a header, OpenAPI | A Go test with a service on `httptest` |
| Failures | An unavailable system, the broker, restarts, a timer deleted during a retry | Go tests that stop a process and a container |
| Load | §12 | `TestLoad` with the bus and the integration service |

Every new test is checked by a mutation: the test must fail if what it checks is removed.

---

## 12. Performance

| Metric | Goal |
|---|---|
| Command throughput on PostgreSQL with the Kafka bus (events and notifications) and the integration service | a drop of no more than 5 % from 2,400 commands/s |
| The latency "commit → a message in Kafka" | p99 ≤ 1 s at this load |
| The latency "commit → the request at the service" | p99 ≤ 200 ms |
| Request delivery | ≥ 5,000 requests/s with 64 workers and a service without pauses; one unavailable system does not change the latency of the others |

---

## 13. Schedule and milestones

Estimates for two engineers, in working weeks, ±30 %.

| Milestone | Stages | Duration | Result |
|---|---|---|---|
| **M1** | I1 | 2 wk | The language, fakes, the end-to-end taxi scenario in memory |
| **M2** | I2, I3 | 2 wk | The delivery queue, the contract, JWT |
| **M3** | I4, I5 | 2 wk | The bus and Kafka; fakes over HTTP |
| **M4** | I6 | 0.5 wk | Load, documentation |

**Total:** about 6.5 weeks. End to end before complete: at the end of M1 taxi goes through the order path on fakes in `reasonr test`.

---

## 14. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| The fake cascade changes the meaning of existing tests | Low | A fake is inactive until initialized (decision 9); a run of every example in I1 |
| An endless exchange between a fake and the model | Medium | A limit of units per step, a `fault` with the chain in the report |
| The `timeout` syntax change breaks models | Medium | An error with a fix; in the repository only taxi has requests with `timeout` |
| The test program with fakes and the production one diverge | Medium | Fakes only add types; `reasonr check` checks both; a test compares the production program's schema with the test one's without fakes |
| franz-go needs a Go newer than 1.23 (as happened with pgx) | Medium | Pick a compatible version at the start of I4; otherwise `segmentio/kafka-go` |
| A token with the tenant `*` gives access to every tenant | Medium | Only roles of external systems; C0011; in the README — a token per system, TLS |
| Duplicates on the bus after a dispatcher failure | High (expected) | `ce_id` for deduplication at the consumer, described in the README |
| Tests with Kafka are flaky in Docker | Medium | Waiting for the broker to be ready, unique topics per test, skipping without `REASONR_TEST_KAFKA` |

---

## 15. Acceptance criteria

1. The end-to-end taxi scenario on fakes (§8) passes `reasonr test --engine memory` and `--engine http` on memory, PostgreSQL and MongoDB without manual system responses.
2. Requests are delivered in parallel and independently; delivery stops by the `timeout` timer; `RunRequests` passes on three stores.
3. The service contract is described by an OpenAPI from the model; JWT in both directions; a service that serves several tenants answers into its own tenant.
4. The bus publishes every event and, if configured, notifications in CloudEvents; `RunPublisher` passes on `memory` and Kafka.
5. The language diagnostics and C0010–C0012 exist; the load measurement is done, the goals of §12 are met or the deviations are described in the progress.
6. The RFC is updated (§16); the README and `AGENTS.md` describe fakes, the request timeout, the contract and the bus.

---

## 16. What changes in the RFC

After the decisions of §2 are approved:

- **§6.4:** the keyword `fake`.
- **§13:** notifications may be published to the bus (an engine setting).
- **§15:** a timer as the timeout of an external request.
- **§16, item 4:** a projection row is written whole; the row version and the deletion mark (decisions 21, 22).
- **§18:** the system role `external(role)`; the parameter `timeout Timer.Id` instead of `timeout 30s`; the integration service contract and JWT — by reference to §31; taxi in the examples of §25.3, §25.5.
- **A new section "Fakes of external systems":** the rules of §3.3, execution as in §7.
- **§27.1:** fakes in tests, the cascade inside a step, the failure kind `fake`; the line "external systems are replaced by a stub" is refined.
- **§28.2:** the grammar of `externalDecl`, `requestDecl`, `fakeDecl`.
- **§31:**
  - the row "Bus" — CloudEvents, every event, notifications if configured, Kafka is the first adapter;
  - the row "Integration service" — the delivery queue, JWT;
  - §31.1 — `bus`, `integrations` (`retry`, `auth`), the event store is required with a bus.
- **Terminology:** "event journal" → "event store" throughout the RFC.
- **§33:** `reasonr build --integrations-openapi`, `reasonrd requests retry`.
- **§38:** stage 5 — a reference to this plan.
