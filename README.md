# ReasonR engine

The engine runs ReasonR models (see the [compiler](https://github.com/tycheg/reasonr-compiler) and its RFC): storage, transactions, delivery, the HTTP API, MCP, the app API, encryption of personal data, tenant isolation. This repository is the engine core:

- the contract of plugins `port` — a Go module of its own without dependencies: the ports of adapters, their registration, the settings node and the byte-level conformance suites;
- the contract of engine kinds `kind` (`Kind`, `Host`, the kernel, the value-level stores), the kernel and the host every engine kind runs on;
- the built-in adapters: memory, HTTP, authentication (JWT, API keys), telemetry; the test drivers `memory`, `api` and `mcp`;
- the server (`server.Main`) and the builder `reasonr-build`: a binary from a manifest of plugins.

The standard `reasonrd` is a manifest of the distribution ([reasonr-dist](https://github.com/tycheg/reasonr-dist)): the core with the official plugins ([PostgreSQL](https://github.com/tycheg/reasonr-postgres), [MongoDB](https://github.com/tycheg/reasonr-mongodb), [Kafka](https://github.com/tycheg/reasonr-kafka), the [Vault](https://github.com/tycheg/reasonr-vault) KMS), the engine kind [stateless](https://github.com/tycheg/reasonr-stateless), and the [web](https://github.com/tycheg/reasonr-web) bundle and test driver. A platform builds its own binary from a manifest of the plugins it needs (`reasonr-build`), as the pilot platform does. Rules for AI agents that test a model on the engine — [AGENTS.md](AGENTS.md).

Active plans — [docs/plans](docs/plans): performance tests (0017), test run workers (0018), the engine part of the scale tests (0015, S4); the framework roadmap is in the [compiler](https://github.com/tycheg/reasonr-compiler/blob/main/docs/roadmap.md).

## Install

```bash
go install github.com/tycheg/reasonr-dist/cmd/reasonrd@latest         # the standard server
go install github.com/tycheg/reasonr-engine/cmd/reasonr-build@latest  # the builder
```

In the examples below `taxi/src` is a model module — for example `examples/taxi/src` of the compiler repository.

## Server

Without `--config` everything is kept in process memory, HTTP is on `:8080`; configuration examples are in the distribution: `engine.example.yaml` (in memory), `engine.postgres.yaml`. With PostgreSQL the server checks at startup that the model is compatible with the stored data (RFC §32) and creates the missing tables and indexes.

```bash
reasonrd dev taxi/src                    # local debugging: test mode, everything in memory, a tenant per extension, fakes; *.rr changes are picked up
reasonrd dev --config engine.yaml --integrations Matching=http://localhost:8090 --ui http://localhost:5173 taxi/src
reasonrd --model taxi/src --test         # test mode: Authorization: Test actor=Customer:<id> | Test role=admin
REASONR_MASTER_KEY=$(openssl rand -base64 32) \
reasonrd --config engine.example.yaml --model taxi/src --roles api,timers
curl -X POST localhost:8080/api/Customer/<uuid>/SignUp -H 'Authorization: Test actor=Customer:<uuid>' -d '{"phoneNumber":"+79990000000"}'
curl localhost:8080/openapi.json                          # the OpenAPI of the model with the extension
curl localhost:8080/openapi/integrations.json             # the integration service contract (also reasonrd openapi --integrations)
curl localhost:8080/.well-known/jwks.json                 # the public keys of the engine's JWT for the integration service
reasonrd migrate --config engine.yaml --model taxi/src --run fillOrderMart    # a manual migration (RFC §32)
reasonrd topics --config engine.yaml --model taxi/src --format json          # the Kafka bus and request topics
reasonrd timers retry --config engine.yaml --model taxi/src                  # bring dead timers back
reasonrd dispatch retry --config engine.yaml --model taxi/src                # deliver dead letters again
reasonrd check --config engine.yaml taxi/src             # the configuration against the models (also at reasonrd startup)
```

Tests, demos and load:

```bash
reasonrd test taxi/src bank/src                          # acceptance tests on the in-memory engine
reasonrd test -v --run "unique" taxi/src                 # steps, notifications, requests
reasonrd test --driver api taxi/src                      # the same tests through the API: a test mode server in process (--url — a running one)
reasonrd test --driver mcp taxi/src                      # through MCP tools
reasonrd test --affected --base origin/main taxi/src     # only the affected tests (RFC §27.4)
reasonrd test --serve --listen :7070 taxi/src            # a run coordinator (RFC §27.3, workers)
reasonrd test-worker --coordinator http://localhost:7070 --drivers memory,api   # a worker: takes tests while there are any
reasonrd demo list taxi/src                              # demo scenarios (RFC §27.5)
reasonrd demo load "A week of rides in Moscow" --tenant demo --config engine.yaml taxi/src   # fill a tenant with demo: true
reasonrd perf smoke taxi/src                             # the performance test peak.perf.rr (RFC §27.6)
reasonrd perf capacity --config engine.yaml taxi/src     # the rate limit on the configuration's adapters
reasonrd mcp --stdio --as actor=Customer:<id> taxi/src   # MCP for an agent (Claude Code, MCP Inspector)
```

`reasonrd test --driver web` is the test driver of the [web repository](https://github.com/tycheg/reasonr-web) that `reasonrd` links (`kind.RegisterDriver`); the browser — `reasonrd web install`. The core does not depend on a browser.

## Codecs

A codec is the serialization format of model values. The built-in one is `json`, the canonical JSON (RFC §30); others are plugins that register themselves with `registry.RegisterCodec` (`port/registry`) and are linked into a binary by its manifest. A codec is chosen where values leave the engine:

```yaml
ingress:
  - { type: http, listen: ":8080", codecs: [msgpack] }  # the model API: arguments by Content-Type, answers by Accept; json always
bus: { type: kafka, brokers: [kafka:9092], codec: msgpack }                # message bodies; content-type is the codec's media type
integrations:
  default: { type: http, url: http://integrations:8090, codec: msgpack }  # request bodies
```

The stores keep the canonical JSON. The app API, MCP, WebSocket and the test API are JSON protocols. A codec gets a value from the model as calls of `port.ValueWriter` (an object, a field with its number, a list, a scalar) and gives one back through `port.ValueReader`, so it does not depend on the compiler.

## Integrations

The engine writes a model's external requests (RFC §18) to the main store in the same transaction as the unit of work. The `dispatch` role sends them to the integration service in parallel, each request with its own attempts:

```yaml
integrations:
  workers: 64                       # concurrent sends of every tenant and system
  retry: { initial: 1s, multiplier: 2, max_interval: 1m, max_elapsed: 1h }
  auth: { key_env: REASONR_INTEGRATIONS_KEY, kid: "2026-10" } # EC P-256, PKCS #8 PEM; also: [{ key_file, kid }] — during rotation
  default: { type: http, url: http://integrations:8090, timeout: 10s }
  Payments: { type: kafka, brokers: [kafka:9092], topic: "reasonr.request.{system}" } # a system of its own — a transport of its own
```

**The request.** `POST {url}/requests/{System}/{request}`. The body is the arguments in canonical JSON (or in the transport's `codec`, with its `Content-Type`) without the `timeout` parameter. Headers:

| Header | Value |
|---|---|
| `Authorization` | `Bearer` with the engine's JWT: ES256, `iss` — `issuer` (`reasonr` by default), `aud` — the system, `tenant`, `jti` — the idempotency key, a 5-minute lifetime; public keys — the engine's `GET /.well-known/jwks.json` |
| `Idempotency-Key` | the same for every attempt: the service drops repeats by it |
| `X-Reasonr-Tenant` | the data tenant |
| `X-Reasonr-Deadline` | the time of the request's `timeout` timer; absent for a request without a timer |
| `X-Reasonr-Attempt` | the attempt number, from 1 |

The engine builds the OpenAPI description of the contract from the model: `reasonrd openapi --integrations` or `GET /openapi/integrations.json`. It also lists the answering commands available to the system role.

**Attempt outcomes.**
- A 2xx response — the request is delivered, and the service carries it out.
- Any other response or a network error — a retry with the same key by `retry`. A request with a `timeout` timer is retried while the timer is alive. Once it fired or was deleted, the request is no longer sent: the model reacts to the missing answer with the timer command. A request without a timer is dropped after `max_elapsed` from commit, with a warning in the log.
- The order of requests is not kept, and after a fault a delivered request may repeat.

**The system's answer** is an ordinary API command with the system role and its JWT. For a service that serves every tenant, the IdP issues a token whose tenant is the `any` value (`auth: jwt`, `tenant: { claim: tid, any: "*" }`). Then the tenant comes from `X-Reasonr-Tenant`, and such a token may carry only roles of the model's external systems.

**Metrics:** `reasonr_requests_total{system, outcome}` (`delivered`, `retry`, `expired`, `forgotten`, `dropped`) and `reasonr_request_delivery_seconds{system}` — from commit to delivery.

**A requirement for the service:** answer fast and stay available — accept a request (2xx) and carry it out on its own. The `timeout` of the `http` transport is the deadline of one attempt, and the sends are shared by every system. An immediate refusal (503, connection refused) does not disturb other systems. A service that does not answer holds each send for the whole `timeout` and, with enough traffic, takes every `worker`: the requests of other systems wait.

## Bus

The bus publishes every model event from the event store (`events` is required, otherwise C0013): in position order, at least once, ordered within an entity. While the broker is unavailable the bus cursor stays, and no events are lost. With `notifications: true` notifications are published right after commit, to their own channels and without delivery guarantees. Personal arguments in the bus are `null`.

```yaml
events: audit                       # the event store
bus:
  type: kafka                       # log — to the server log; memory — in process memory
  brokers: [kafka:9092]
  topic: "reasonr.{kind}.{type}"    # {tenant}, {kind}: event | notification, {type}: Order, Customer
  notifications: true
```

A message is CloudEvents 1.0 in binary mode. The value is the arguments in canonical JSON (or in the bus `codec`, with its media type in `content-type`), the record key is the id of the entity or of the notification recipient. The attributes go in `ce_*` headers:

| Attribute | Value |
|---|---|
| `ce_id` | `{tenant}/{position}/{number}`; for a notification — `{tenant}/{position}/n{number}`. It does not change on a repeat: the receiver drops repeats by it |
| `ce_type` | `Order.paymentReceived` |
| `ce_source` | `reasonr/{tenant}/{Type}` |
| `ce_subject` | the id of the entity or recipient; absent for a notification without a recipient |
| `ce_time` | the time of the unit of work |
| `ce_tenant`, `ce_position`, `ce_kind` | the tenant, the position, `event` or `notification` |

The topics are derived from the models: the events of every type with events, notifications, and the external request topics of the systems that go through the `kafka` transport. `reasonrd topics --config engine.yaml --model <module> [--format json]` prints the list without connecting to the broker, so topics can be created in Terraform or GitOps; the JSON has the component and the `create_topics` settings. At startup `reasonrd` checks that every topic exists and does not start without them; if the broker is unavailable, it only warns. The engine creates topics itself only with an explicit `create_topics` setting of the bus or a transport — then `reasonrd migrate` creates the missing ones; `reasonrd --test` always creates them at startup (with one partition without `create_topics`).

## Fakes in `reasonrd --test`

The fakes of external systems (`*.fake.rr`, RFC §18.1) work in ordinary tenants of a test server too — to check scenarios through the API by hand:

```yaml
integrations:
  default: { type: fake }           # only with --test (C0010)
```

```bash
curl -X POST localhost:8080/_test/default/fakes/Matching/Setup -d '{"freeDrivers":["<uuid>"]}'
```

Requests go to the fakes through the delivery queue, and the fakes' calls of model commands run with the system role; the errors of these commands go to the server log. Until a fake is created, a request is only logged. The timers of ordinary tenants in `--test` fire by the system clock; the ephemeral tenants of `reasonrd test --driver api` live by virtual clocks, and the test API runs their fakes as a cascade at the end of a step.

## Personal data

The engine stores `personal` fields and parameters (RFC §20) encrypted. Values are encrypted with AES-256-GCM under the subject key; the subject key is wrapped with the tenant key, and that one with a master key in a KMS, which is not in the database. Search and uniqueness on personal fields go through blind indexes. Ciphertexts are everywhere values go: rows, the outbox, timers, responses by retry key, the event store, asynchronous projections. Values never reach logs or the bus. `forget` in a command destroys the subject key and erases its fields in the same transaction: the data is unreadable everywhere, and its unique values are free at once.

```yaml
crypto:
  kms: { type: vault, address: "https://vault:8200", key: reasonr, token_env: VAULT_TOKEN }
  # kms: { type: local, key_env: REASONR_MASTER_KEY }   # 32 bytes, base64: development and air-gapped setups
  cache: { subjects: 100000, ttl: 5m }
  forget_log: audit        # the forget journal — in a store outside the main database
```

- Without `crypto`, a server with personal data in the model does not start (check C0007). The exception is `--test`: the master key is random, in memory.
- `reasonrd check` warns (C0009) if the forget journal is not kept or is in the main database.
- **Restoring from a backup.** A backup of the main database holds the keys of subjects forgotten after it. So the forget journal lives in another database that is not restored together with the main one. The order: restore the main database; before starting the servers, run `reasonrd pii replay --since <backup time>` — it repeats the forgets written after the backup; start the servers.
- **Master key rotation:** `reasonrd pii rotate --from old.yaml` rewraps the tenant keys with the key from `crypto.kms` of the current configuration (`old.yaml` is the configuration with the previous one). Without `--from` — with a new version of the key of the same KMS, for example after `vault write -f transit/keys/reasonr/rotate`. The data is not re-encrypted, the servers do not stop.
- **Migrations.** When a field becomes or stops being `personal`, `reasonrd migrate` (and the server start) encrypts or decrypts the values in rows and rebuilds the blind indexes. Events already written to the outbox and the journal stay as they were (a limitation, RFC §32).

```bash
reasonrd pii replay --config engine.yaml --model taxi/src --since 2026-10-01T03:00:00Z
reasonrd pii rotate --config engine.yaml --model taxi/src --from engine.old.yaml
```

## Feature flags

New functionality ships to production under a feature flag and rolls out without a restart (RFC §23.4). The engine keeps the flag state and the change journal in the `features` store (the main one by default), and instances learn about changes through the notification hub.

```bash
reasonrd feature list --config engine.yaml --model taxi/src            # the code's flags: date, age, rollout
reasonrd feature on tips --config engine.yaml --model taxi/src --actor Ordering.Customer:<id> --tenant acme-prod
reasonrd feature on tips --config engine.yaml --model taxi/src --role support --tenant acme-prod
reasonrd feature on tips --config engine.yaml --model taxi/src --tenant acme-prod
reasonrd feature on tips --config engine.yaml --model taxi/src --all
reasonrd feature off tips --config engine.yaml --model taxi/src        # switch off at every level
reasonrd feature log --config engine.yaml --model taxi/src             # who, when, what
reasonrd feature on tips --url http://localhost:8080 --all             # a reasonrd dev engine
```

## API

- The model API (RFC §31.3): `POST /api/{Entity}/{id}/{Command}` — a command, notifications to the initiator in the response, `Idempotency-Key` for retries; `GET /api/{Type}/{Query}?…&after=<position>` — a query; `GET /api/ws` (WebSocket) and `GET /api/poll` (long polling) — notifications from other actors; `/openapi.json`; `/healthz`, `/readyz`, `/metrics`.
- The external API contract (RFC §31.3.1): outside test mode `/api` exposes only the published methods; `Reasonr-Version` selects a released version.
- MCP (RFC §31.3.2): `POST /mcp` with `ingress: [{ type: http, mcp: true }]`, always in test mode.
- The app API (RFC §31.4): `/app/…` — manifests, screen descriptions, screen data, place queries and commands; `/ui/…` — the web runtime, served when the binary links the [web bundle](https://github.com/tycheg/reasonr-web) or by `ingress[].ui.dir`.

## Development

Go 1.23+. The core and the contract are two modules of one workspace (`go.work`): the core's tests run on the contract of the same commit, its `go.mod` requires a released one.

```bash
make check                 # gofmt, vet, staticcheck, the tests of the core, the builder and the contract (its suites run in the adapters' tests)
make modules               # the auth module the taxi example requires, into the module cache (make test does it)
make fuzz FUZZTIME=1m      # the fuzz test of the memory engine
make bench                 # benchmarks of the test runs and the memory engine
make install               # reasonr-build into $(go env GOPATH)/bin
make images                # the reasonr-builder image
```

A release is a tag `vX.Y.Z` of the core and `port/vX.Y.Z` of the contract. The engine tests read the example models from the compiler module at the required version (`internal/testmodels`). The end-to-end tests and the runs of the plugins on the example models are the distribution's.

## Contracts

Two contracts, by who needs them (plan 0022). Everything else is under `internal/` (`internal/server.TestPublicPackages` checks it). Each exported identifier is either used outside the engine or has its reason in the [contract audit](https://github.com/tycheg/reasonr-compiler/blob/main/docs/plans/0021-contract-audit.md):

| Package | Module | For whom | What |
|---|---|---|---|
| `port` | `reasonr-engine/port` (no dependencies, tags `port/vX.Y.Z`) | plugins: adapters, codecs, UI bundles, platforms' own adapters | The byte-level ports: row stores, projections, timers, the outbox, events, requests, keys, KMS, coordination, feature flags, the bus (ready messages), integrations (ready requests), the hub link, codecs, metrics, the UI bundle; the settings node and its YAML parser |
| `port/registry` | the same | the same | `Register*` of stores, hubs, buses, transports, KMS, codecs, commands, the UI bundle; `Deps` (`Logger`, `Metrics`, `State`); `Secret`. `Registered` is the engine's read side |
| `port/porttest` | the same | tests of plugins | The byte-level conformance suites: a layout made by hand, no model, no engine |
| `kind` | `reasonr-engine` | engine kinds, the web driver | The kind contract (`Kind`, `Engine`, `Host`, `Maintainer`, `Config`), the executor, the kernel, the value-level stores, the test driver; `RegisterKind`, `RegisterDriver`, `OpenTool` |
| `kind/kindtest` | the same | tests of kinds and of distributions | The model-level suites, the example models, engines from `engine.yaml`, the test kind, test servers |
| `server` | the same | distributions | `Main`, `Run` |

## Plugins

A plugin is a Go module that requires the contract alone (`github.com/tycheg/reasonr-engine/port`) and registers in `init()`:

```go
package oracle

import (
	"github.com/tycheg/reasonr-engine/port"
	"github.com/tycheg/reasonr-engine/port/registry"
)

func init() {
	registry.RegisterStore("oracle", func(n *port.Node, d registry.Deps) (any, error) { return open(n) }, registry.PersonalData)
}
```

Its tests run the suites of `port/porttest` on a live store (`porttest.RunStore`), bus, hub link or KMS. A binary links it by a manifest, built by `reasonr-build` (or the `reasonr-builder` image, without a local Go); the calls are compiled in, as fast as a module added by hand:

```yaml
# components.yaml
module: example.com/acme/engine
binary: acme-engine
engine: github.com/tycheg/reasonr-engine v0.1.0
port: github.com/tycheg/reasonr-engine/port v0.1.0
plugins:
  - github.com/tycheg/reasonr-stateless/src v0.1.0
  - github.com/tycheg/reasonr-postgres/src v0.1.0
  - example.com/acme/reasonr-oracle v1.2.0
replace:                                   # local directories (development)
  example.com/acme/reasonr-oracle: ./oracle
```

```bash
reasonr-build -f components.yaml -o acme-engine
docker run --rm -v "$PWD:/work" reasonr-builder:0.1.0 -f components.yaml -o acme-engine
reasonr-build -f components.yaml --main cmd/acme-engine/main.go   # a module that keeps its own go.mod
```

Every module of a build must require one version of the contract; the builder names the modules that differ.

A plugin from a private repository is fetched the way `go` fetches any private module: `GOPRIVATE` and git credentials with read access (in the image: `-e GOPRIVATE=example.com/acme -v ~/.netrc:/root/.netrc:ro`), or a local checkout with `--replace module=dir`.

## Layout

| Directory | What is in it |
|---|---|
| `port` (a module), `kind`, `kind/kindtest`, `server` | The contracts above; `server` is two functions over `internal/server` |
| `cmd/reasonr-build` | The builder and its image (`Dockerfile`) |
| `internal/server` | The `reasonrd` commands common to the engine kinds: serve, dev, check, migrate, test, openapi, topics, pii, feature, api usage, mcp, demo, perf, test-worker |
| `internal/kernel` | The engine kernel: tenant programs, read routing, commit with retries, uniqueness, idempotency, notifications, timers, migrations, `after` |
| `internal/dispatch` | The effect stream: cursors, leases, retries, dead letters; asynchronous projections, the event store; the bus from the event store (`Bus`) and notifications without guarantees (`Notifier`); parallel sending of external requests with retries (`Sender`) |
| `internal/rows` | Lays a model out for a row store of another repository (the byte-level ports of `port`): tables and key columns, encryption and blind keys, the outbox, timer and request records, schema rewrites |
| `internal/storekit` | The shared parts of `rows` and the in-memory store: paths, search conditions and order, cursors, projection operations, encryption of records, the outbox and responses by key |
| `internal/pii` | Personal data keys: the `memory` and `local` KMS, tenant and subject keys with a cache, AES-256-GCM, blind indexes, master key rotation |
| `internal/bus` | The bus message format: CloudEvents 1.0, binary mode; ready messages |
| `internal/codecs` | The built-in `json` codec and the walker of values for codec adapters |
| `internal/config`, `internal/builtin`, `internal/registry`, `internal/kindreg` | `engine.yaml`; the built-in adapters; the engine's registry (the built-in adapters with the full `Deps`, the plugins of `port/registry`, the kinds and drivers of `kind`) |
| `internal/ports` | The engine's own interfaces no module outside uses: the ingress and authentication, tracing, the hubs of forgotten subjects and rollout changes, the value-level stores only the engine implements, the request signer |
| `internal/host` | The host of an engine kind: building the shared part from the configuration (stores, ingress, authentication, the hub, the bus, the test API), the kind opened on it (`kind.Host`) — the kind gives the executor and its roles, the host's are api and dispatch (the bus) |
| `internal/hub` | The notification hub of several instances over a link (`port.HubLink`) |
| `internal/check` | `reasonrd check`: the engine configuration against the models |
| `internal/testapi` | The server's test mode: `/_test/*`, ephemeral tenants, virtual clocks |
| `internal/features` | The feature flag rollout |
| `internal/conformance` | The model-level conformance suites (`kind/kindtest` gives them to other modules) |
| `internal/adapters/memory` | In-memory adapters (store, hub, event store, clocks) and the engine for `reasonrd test` |
| `internal/adapters/http` | HTTP ingress: the model API, WebSocket, long polling, OpenAPI, MCP (`httpapi`); the app API `/app/…` and the runtime `/ui/…` (`appapi`); the transport to the integration service (`integration`) |
| `internal/adapters/auth`, `internal/adapters/telemetry` | JWT, API keys and test authentication; `slog` logs and Prometheus metrics |
| `internal/testrun` | `reasonrd test`: running tests on the in-memory engine and through a server's API, workers, text and JSON reports |
| `internal/perf`, `internal/transfer`, `internal/servertest`, `internal/testmodels` | Performance tests; state transfer between kinds; test servers; the example models |
