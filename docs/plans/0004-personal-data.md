# Plan: ReasonR personal data — encryption, blind indexes, `forget`

| | |
|---|---|
| **Basis** | [RFC-0001](https://github.com/tycheg/reasonr-compiler/blob/main/docs/rfc/0001-reasonr-language/README.md), stage 4 of the implementation plan (§38): "Encryption, blind indexes, `forget`, `reasonr pii`", criterion — crypto-shredding tests on memory and http; RFC §20 (marking and `forget`), §31 (a key per subject, AES-GCM, KMS, HMAC), §32 (a field became `personal`); decision 16 of the engine plan ([0003-engine.md](0003-engine.md)) |
| **Status** | Approved 2026-10-04 with the decisions of [§2](#2-decisions-to-approve). Done; progress — [0004-personal-data-progress.md](0004-personal-data-progress.md) |
| **Date** | 2026-10-04 |
| **Implementation language** | Go 1.23+; cryptography — the standard library only (`crypto/aes`, `crypto/cipher`, `crypto/hmac`, `crypto/sha256`); `crypto/hkdf` arrived in Go 1.24, so HKDF (RFC 5869) is our own, a few lines on top of HMAC |
| **Note** | Paths, package names and RFC section numbers are those of the time (the repository `tycheg/reasonr` before the split) |

---

## Contents

1. [Goal and scope](#1-goal-and-scope)
2. [Decisions to approve](#2-decisions-to-approve)
3. [Threat model](#3-threat-model)
4. [Cryptography](#4-cryptography)
5. [Where a subject's data lives](#5-where-a-subjects-data-lives)
6. [Ports and the library](#6-ports-and-the-library)
7. [`forget`](#7-forget)
8. [Compiler and interpreter](#8-compiler-and-interpreter)
9. [Configuration and checks](#9-configuration-and-checks)
10. [Stages P1–P7](#10-stages-p1p7)
11. [Migrations and operations](#11-migrations-and-operations)
12. [Testing](#12-testing)
13. [Performance](#13-performance)
14. [Schedule and milestones](#14-schedule-and-milestones)
15. [Risks](#15-risks)
16. [Acceptance criteria](#16-acceptance-criteria)
17. [What changes in the RFC](#17-what-changes-in-the-rfc)

---

## 1. Goal and scope

**Goal:** personal data is stored only encrypted with its subject's key; `forget` destroys the key, and the subject's data becomes unreadable everywhere it went: in the state, events, projections, the outbox, the event journal, timers, responses by idempotency key, backups. Search and uniqueness over personal fields work through blind indexes.

**What already exists** (stages 1–3):

| Part | Where | State |
|---|---|---|
| The `personal` marking, `forget`, leak analysis E0901–E0905, W1020, W1021 | `compiler/check/pii.go`, `compiler/check/queries.go` | Done |
| `reasonr pii` — a map of subjects, fields, flows and the places of `forget` | `compiler/tools/pii.go` | Done, extended in P2 |
| The subject in the schema: `FieldInfo.Personal{Subject, SubjectType}` | `schema.go` | Done |
| `UnitOfWork.Forget`, `forget` in the outbox | `uow.go`, `engine/storekit/payload.go` | Done |
| Imitated `forget`: the subject's personal fields are overwritten with none in **every** row of the tenant | `engine/memory/forget.go`, `adapters/postgres/commit.go`, `adapters/mongodb/commit.go` | Replaced: data in plain text, a full scan of rows, events in the outbox are not erased |
| The `KeyStore` port (`Key`, `Destroy`) | `engine/port/effects.go` | Replaced ([§6](#6-ports-and-the-library)) |

**In the plan:**

- the library `engine/pii`: the key hierarchy, value encryption, blind indexes, a key cache;
- the `KMS` port and the adapters `memory`, `local`, `vault` ([decision 12](#2-decisions-to-approve));
- storing subject keys in the main store, `forget` in the commit transaction — in memory, PostgreSQL, MongoDB;
- encryption in every place of [§5](#5-where-a-subjects-data-lives): the state, synchronous and asynchronous projections, the outbox, the event journal, timers, responses by key, dead letters;
- the compiler and the VM: the subjects of values leaving a command (external requests, notifications, errors), a rule for projection operations;
- masking personal arguments in logs;
- the migration "a field became `personal`" and back, the first start on old data in plain text;
- operations: master key rotation, the `forget` log and replaying it after a restore from a backup, deleting a tenant;
- crypto-shredding tests on memory and http (memory, PostgreSQL, MongoDB), a load test with encryption.

**Not in the plan:**

- giving a subject their data and export (the right of access, GDPR art. 15) — backlog;
- rotating the blind index key (needs reindexing every row) — backlog;
- padding the ciphertext length (the value length is visible) — backlog;
- AWS KMS and GCP KMS adapters — a backlog modeled on E6 ([decision 12](#2-decisions-to-approve));
- the Kafka bus and the integration service (stage 5) — here only the rule of what goes outside ([decision 11](#2-decisions-to-approve)).

---

## 2. Decisions to approve

| # | Question | Proposed decision | Alternative |
|---|---|---|---|
| 1 | Where to encrypt | **In the engine**, in a shared library the store adapters call: ciphertext is what reaches the database and the outbox. The model only says "whose data" (RFC §20.1) | Database encryption (TDE, `pgcrypto`) — no crypto-shredding per subject, each database has its own |
| 2 | The key hierarchy | **Three levels:** a master key in KMS → a tenant key (32 bytes, stored wrapped with the master key, cached decrypted in the process) → a subject key (32 bytes, stored wrapped with the tenant key). KMS is called once per tenant and process, not per subject | The subject key is wrapped directly in KMS — a KMS call on every cache miss, the cost and latency of KMS on the command path |
| 3 | Where subject keys are stored | **In the main store** (`rt_subject_keys`): creating a key and `forget` are atomic with the data; `forget` deletes the key in the commit transaction of its unit of work | A separate key store (Vault KV) — `forget` only after commit through the outbox, a window in which forgotten data can still be read |
| 4 | What one ciphertext covers | **Each personal value separately:** a field, an event argument, a timer argument; a value object and a collection — whole (RFC §20.2, item 5). In the canonical JSON the value is replaced by an object `{"$p": "<base64url>"}`: field names in the language do not start with `$`, so a ciphertext cannot be mistaken for a plain value | One envelope per row and subject — fewer overhead bytes, but a partial projection upsert and reading one field need decrypting the whole envelope |
| 5 | Algorithms | **AES-256-GCM** with a random 96-bit nonce; AAD — the tenant, the subject's type and id, the key version, the owner type and the member name (a ciphertext cannot be moved to another field or another subject). The blind index — **HMAC-SHA-256** truncated to 16 bytes. Derived keys — HKDF-SHA-256. The Go standard library only | XChaCha20-Poly1305 or AES-GCM-SIV — an external dependency (`x/crypto`) in the kernel, which contradicts decision 6 of the engine plan |
| 6 | The blind index key | **Its own per tenant and personal field** (type + field name), derived by HKDF from the tenant's blind index root key. Equal phone numbers in different tenants and different fields give different index values. Search — `==` and `in` (E0905); page cursors carry blind values, not plain ones | One key per tenant — simpler, but equal values in different fields and types are comparable |
| 7 | A subject's data after `forget` | **A subject may get data again:** `forget` destroys the current key; the next write of the subject's personal data creates a key of a new version. Old ciphertexts (the version is in the ciphertext) read as none forever. So a client who deleted their data can sign up again | A tombstone: after `forget` the subject's data is never stored again (none is written) — unexpected for a model that accepts `SignUp` again |
| 8 | A race of `forget` with a concurrent write | **A strict check in commit:** a unit of work that writes a subject's data reads the versions of their keys in its transaction (`for share` in PostgreSQL, under a lock in memory, touching the key document in MongoDB) and compares them with the versions it encrypted with; a mismatch is `ErrConflict` and the kernel's usual retry. After `forget` no write with the old key commits | A deferred second sweep after N seconds — simpler, but for N seconds after `forget` blind values stay in the indexes, and a write in that window is lost |
| 9 | What `forget` erases right away | **The subject's key and the rows that hold their data:** the subject's personal fields become none, blind indexes null. The rows are found by a `subjects` column (the list of a row's subjects with a GIN or multikey index), not by a scan. So reads return none right away regardless of other instances' caches, and unique values are released (RFC §20.3). The outbox, the event journal and backups are not rewritten: destroying the key protects them | Only destroy the key — cheaper, but blind indexes keep uniqueness (a new client cannot take the forgotten one's phone number) and find the rows |
| 10 | The subject of values leaving a command | **The compiler knows the source** of every personal value (leak analysis): in a command it is one's own field, one's own parameter or an event argument of a reaction, so the subject is computable from `self`. For the arguments of external requests, notifications and errors the compiler emits the subject of the call site, the VM puts it into the unit of work (`Subjects`), and the engine encrypts the outbox and responses by key with them. A value with several subjects is encrypted with each key in turn. A projection operation that sets a personal field must also set the field of its subject — a new error **E0906** | Not storing notifications and errors in responses by key (a command retry gets only the status and the position), and encrypting requests with the key of the sending entity — simpler, but a client's `forget` would not reach their address in an order's request |
| 11 | What goes outside | **The integration service** gets decrypted values (it is a data processor; the channel is TLS); if the subject is forgotten before sending, a personal argument is none, and a request with a non-optional personal parameter is not sent (a log line, a metric; the model handles the timeout). **The bus** publishes events without personal arguments (none): its storage is out of reach of `forget`. **Notifications** and query results — plain text to an authorized client, not stored (RFC §13, §20.2) | Publishing ciphertext to the bus — consumers need access to the keys; publishing plain text — a `forget` violation |
| 12 | KMS adapters in this stage | **`memory`** (tests, `--test`), **`local`** (a master key from an environment variable or a file; development and closed environments), **`vault`** (HashiCorp Vault Transit over HTTP, no dependencies, a test in Docker). AWS KMS and GCP KMS — backlog modules | AWS KMS right away (the example in RFC §31.1) — a module on `aws-sdk-go-v2` (a Go version risk, as with pgx), a test through LocalStack |
| 13 | Backups | **A `forget` log** (the subject's type and id, the position, the time) is written by an effect stream consumer into a store separate from the main one (`crypto.forget_log`). The command `reasonrd pii replay` destroys the keys again by the log after the main database is restored from a backup. `reasonr engine check` warns if the log is in the main store | Only document it: a restored backup "resurrects" the forgotten until the next `forget` |
| 14 | The in-memory engine (`reasonr test`, `--engine memory`) | **It encrypts the same way,** with the `memory` KMS: one code path, and the crypto-shredding tests on memory check real keys, not an imitation | Keep the imitation in memory — faster tests, but the RFC criterion "crypto-shredding tests on memory" would test the wrong thing |
| 15 | Logs and traces | Personal parameters of commands and queries are **masked** (`"***"`) in every log, incident and trace attribute, test mode included. The mask follows the schema (the parameter's `Personal`) | Not logging arguments at all — debugging of non-personal ones is lost |
| 16 | The key cache | Decrypted subject keys — an in-process LRU (100,000 by default, TTL 5 min), tenant keys — TTL 1 h. After `forget` the instance broadcasts an eviction over the hub. Correctness does not depend on the cache (decisions 8, 9) | No cache — reading `rt_subject_keys` on every load |
| 17 | Old data in plain text | On the first start with encryption `reasonrd migrate` encrypts the existing personal values and builds the blind indexes, the same as for "a field became `personal`" (RFC §32). The table `rt_forgotten` moves into the `forget` log, and its subjects get a tombstone key version | Requiring an empty database — breaks deployed environments |

---

## 3. Threat model

| Threat | Protection |
|---|---|
| A leaked database dump or backup, replica, outbox, event journal | Ciphertext; subject keys are wrapped with the tenant key, and that one with the master key in KMS, which is not in the dump |
| Personal data in logs, metrics, traces | A mask by schema (decision 15); values do not get into metric names |
| Analysts and bus consumers | A bus without personal arguments (decision 11); the event journal is ciphertext |
| "Forget" a subject everywhere | Destroying the key + erasing rows (decisions 8, 9) + the `forget` log for backups (decision 13) |
| Tampering with a ciphertext (moving it to another field or another subject) | AAD (decision 5): decryption fails |
| A dictionary attack on a blind index of phone numbers | The HMAC key is not stored next to the data: it is derived from the tenant key wrapped in KMS |

**Does not protect** (limitations, recorded in RFC §20):

- from a compromised application server or whoever has access to the KMS: they have the keys;
- from implicit flows (`if phone == …`), which the language does not track (RFC §20.2, item 2);
- from metadata: ids, times, value lengths, equality of values of one field (the blind index is equal for equal values);
- from copies at recipients: the integration service and clients get plain text.

---

## 4. Cryptography

### 4.1. Keys

```
KMS: the master key (never leaves the KMS; for local — 32 bytes from the environment)
 └─ tenant key T: 32 bytes, rt_tenant_keys.wrapped = KMS.Wrap(T, context = tenant)
     ├─ wrapping key W = HKDF(T, "reasonr/wrap")
     ├─ blind index root B = HKDF(T, "reasonr/blind")
     │   └─ field key = HKDF(B, "<Type>.<field>")
     └─ subject key K (Type, id, version): 32 random bytes,
        rt_subject_keys.key = AES-GCM(W, K, aad = tenant|Type|id)
```

- A subject key is created on the first write of their data: `insert … on conflict do nothing` outside the commit transaction, then reading the winner. An extra key (the commit did not go through) is harmless.
- A tenant key is created with the data tenant or on the first write. Deleting the tenant key crypto-shreds the whole tenant (ephemeral test tenants, deleting a client).
- Master key rotation rewraps only tenant keys (`reasonrd pii rotate`). The tenant key is not rotated: the blind indexes are derived from it (backlog).

### 4.2. A value's ciphertext

`{"$p": base64url(key version: uint32 ‖ nonce: 12 bytes ‖ AES-GCM(K, the canonical JSON of the value, aad))}`, where `aad = tenant ‖ subject type ‖ subject id ‖ version ‖ owner type ‖ member name`.

- The plain text is the canonical JSON (§8.2 of the engine plan), so strings, value objects and collections are encrypted the same way.
- A failed decryption (no key, another version, a wrong tag) gives none. A wrong tag with an existing key is also an incident in the log and a metric: it is corruption or tampering.
- Overhead: 32 bytes per value plus base64 (+33 %).

### 4.3. The blind index

`HMAC-SHA-256(field key, the canonical JSON of the value)[:16]`. The index key column of a personal field holds these 16 bytes (`bytea` in PostgreSQL, `BinData` in MongoDB, a string in memory). The kernel computes the blind values for `==` and `in` conditions, cursors and the uniqueness check (`engine/kernel/unique.go`); the adapter compares bytes. There is no sorting by personal fields (E0905), so the order of blind values reveals nothing.

### 4.4. Cache and eviction

`pii.Keyring` keeps the decrypted tenant and subject keys and ready `cipher.AEAD`s (no allocations on repeated encryption). A cache miss is one query to `rt_subject_keys` for all the subjects of a unit of work. `forget` broadcasts an eviction over the notification hub as a service message; without a hub (one instance) — a local eviction.

---

## 5. Where a subject's data lives

| Place | How it is stored | Subject | After `forget` |
|---|---|---|---|
| Entity state | Ciphertext in `state`; the `subjects` column; blind key columns | From the schema's `Personal`, by the state after the unit of work | Fields become none, blind columns null, version +1 (in the `forget` transaction) |
| A synchronous projection | The same | By the subject field of the projection row (E0906 guarantees it is set) | The same |
| An asynchronous projection (OpenSearch, a separate PostgreSQL) | Ciphertext and blind values, written by the consumer | The same | `ProjectionStore.Apply` gets the unit of work's `forget` and erases rows by `subjects` |
| Event arguments in the outbox | Ciphertext in `payload` | The event's entity or the `personal(...)` path | Unreadable: the key is destroyed |
| The event journal | Arguments as in the outbox, not decrypted | — | Unreadable |
| Asynchronous projection operations in the outbox | Ciphertext | By the subject field in the operation | Unreadable |
| External requests in the outbox and in dead letters | Ciphertext | From the unit of work (decision 10) | Unreadable; on sending — none or a refusal (decision 11) |
| Timer arguments (`rt_timers.args`) | Ciphertext | The entity the timer's command is called on | Unreadable: a personal argument is none; a non-optional one — the timer does not run, an incident "subject forgotten", the timer is deleted |
| The response by idempotency key | The arguments of notifications and errors with subjects are ciphertext | From the unit of work (decision 10) | A command retry gets none in these arguments |
| Publishing to the bus | Personal arguments are none | — | — |
| Logs, traces | The `***` mask | — | — |
| API responses, notifications, query results | Plain text to an authorized client, not stored | — | The next read gives none |

---

## 6. Ports and the library

### 6.1. `engine/pii` (the kernel, no dependencies)

```go
// Keyring holds tenant and subject keys with a cache; one per process.
type Keyring struct { /* KMS, KeyStore, LRU */ }

func NewKeyring(kms port.KMS, keys port.KeyStore, opts Options) *Keyring

// Sealer encrypts one unit of work or one read: it collects the versions of
// the keys used, which commit checks in the transaction.
func (k *Keyring) Sealer(ctx context.Context, tenant port.Tenant) *Sealer
func (s *Sealer) Seal(subj reasonr.Subject, owner reasonr.TypeRef, member string, plain []byte) ([]byte, error)
func (s *Sealer) Open(subj reasonr.Subject, owner reasonr.TypeRef, member string, sealed []byte) (plain []byte, ok bool, err error)
func (s *Sealer) Blind(t reasonr.TypeRef, field string, canonical []byte) [16]byte
func (s *Sealer) Used() []KeyVersion // for the check in commit (decision 8)
func (k *Keyring) Forgotten(tenant port.Tenant, subjects []reasonr.Subject) // cache eviction
```

`engine/storekit` gets a codec with encryption: `SealRecord`/`OpenRecord` by the `Personal` of fields and parameters, `Subjects(record)` for the `subjects` column, blind values in `SortKeys`, `UniqueKey`, cursors; `EncodePayload`/`DecodePayload` encrypt events, projection operations, requests, responses. The imitating `Erase` and `rt_forgotten` are removed.

### 6.2. Ports

```go
// KMS is the master key: it wraps tenant keys (adapters memory, local, vault).
type KMS interface {
	Wrap(ctx context.Context, keyContext string, plain []byte) ([]byte, error)
	Unwrap(ctx context.Context, keyContext string, wrapped []byte) ([]byte, error)
}

// KeyStore holds the wrapped keys of tenants and subjects; the main store
// implements it (decision 3). Destroying keys is part of Commit (Work.Forget).
type KeyStore interface {
	TenantKey(ctx context.Context, tenant Tenant) (wrapped []byte, err error)       // nil — not yet
	CreateTenantKey(ctx context.Context, tenant Tenant, wrapped []byte) ([]byte, error) // the race winner
	SubjectKeys(ctx context.Context, tenant Tenant, a *reasonr.Arena, s []reasonr.Subject) ([]WrappedKey, error)
	CreateSubjectKey(ctx context.Context, tenant Tenant, a *reasonr.Arena, s reasonr.Subject, wrapped []byte) (WrappedKey, error)
}
```

- `UnitStore` is created with a `*pii.Keyring` (through the registry, like codecs), encrypts on `Commit`, decrypts on `Load`, `Find`, `Remembered`, `Lease`.
- `Unit` gets `Keys []pii.KeyVersion` — the versions commit checks in the transaction (decision 8).
- `ProjectionStore.Apply` gets the `forget []reasonr.Subject` of the same position.
- `Capabilities.Personal` — the adapter stores personal data (the `subjects` column, blind indexes). `reasonr engine check` does not allow a projection with personal fields in a store without this capability.

---

## 7. `forget`

### 7.1. In the commit transaction

The order of steps in `Commit` (after writing states, projections, timers):

1. Check the versions of the keys the unit of work encrypted with (`Unit.Keys`); a mismatch is `ErrConflict`.
2. For every subject of `Work.Forget`: delete the `rt_subject_keys` row (in PostgreSQL the lock waits for the commits holding `for share`).
3. Find the rows of the main store with this subject in `subjects` (only types with a `Personal` in the schema), erase their personal fields and blind columns, increment the entity versions (a concurrent command gets a conflict and rereads).
4. Write `forget` into the outbox (already there) and call `Keyring.Forgotten` after commit.

On an error in the unit of work none of this runs (RFC §20.3). `forget` acts on everything that exists after commit, including data written by the same unit of work.

### 7.2. After commit

- The asynchronous projection consumer passes `forget` to `ProjectionStore.Apply`.
- The `forget` log consumer writes records into `crypto.forget_log` (decision 13).
- The bus consumer does nothing: there are no personal arguments there.

### 7.3. MongoDB

MongoDB transactions do not detect a read-write conflict, so step 1 is not a read but a conditional update of the key document (`{_id, v: version}` → `$set: {seen: position}`): `forget` deletes the document, and a concurrent commit gets a write conflict. The cost is a write to the key document of a subject whose data the unit of work writes; there are no hot subjects (every client has their own key).

---

## 8. Compiler and interpreter

| Change | Where | Why |
|---|---|---|
| The call site's subject for the arguments of external requests, notifications and errors with a personal label: a type and a path from `self`, computed at the moment of the call | `compiler/check/pii.go` (the labels exist), `lower`, `codegen`, a new VM operation | Decision 10 |
| `ExternalRequest.Subjects`, `Notification.Subjects`, `BusinessError.Subjects` — the subjects by argument | `uow.go`, `runtime/vm`, `engine/kernel/determinism.go` | Encrypting the outbox and responses by key |
| **E0906:** a projection operation sets a personal field but not the field of its subject | `compiler/check/pii.go`, `compiler/diag`, an explanation in `diag explain` | An asynchronous operation cannot be encrypted without knowing the subject |
| `reasonr pii`: where data is stored (ciphertext, a blind index, a bus without the field), the subjects of values leaving a command | `compiler/tools/pii.go` | The map matches what the engine does |
| The test runner: the `then` step unchanged; new tests in the examples | `examples/taxi`, `examples/bank` | `forget` scenarios (§12) |

The language does not change except for E0906. E0906 does not fire in the taxi and bank examples (by `reasonr pii`: personal data is only in the fields and parameters of `Customer` and `Client`, and does not get into projections).

---

## 9. Configuration and checks

```yaml
crypto:
  kms: { type: vault, address: https://vault:8200, key: reasonr, token_env: VAULT_TOKEN }
  # kms: { type: local, key_env: REASONR_MASTER_KEY }     # 32 bytes, base64
  cache: { subjects: 100000, ttl: 5m }
  forget_log: audit                 # the store of the forget log (decision 13)
```

- With `--test` and without `crypto` — the `memory` KMS with a random master key.
- `reasonr engine check`:
  - **C0007** (an error) — the model has personal data and `crypto.kms` is not set (except with `--test`);
  - **C0008** (an error) — a projection with personal fields in a store without `Capabilities.Personal`;
  - **C0009** (a warning) — `forget_log` is not set or points to the main store: a backup of the main database would bring the forgotten back.

---

## 10. Stages P1–P7

### P1. The library and KMS

- `engine/pii`: HKDF, value encryption, blind indexes, `Keyring`, `Sealer`, an LRU; the ports `KMS`, `KeyStore`.
- The `memory` and `local` KMS; the `crypto` registry.
- Tests: AES-GCM and HMAC vectors, round-trip encryption of every value type, AAD tampering, a wrong version, concurrent key creation, fuzzing of decryption.

**Done when:** the library is covered by tests, and encrypting a repeated value does not allocate (a benchmark).

### P2. Compiler and VM

- The subjects of values leaving a command (§8), E0906, `reasonr pii`.
- **Done when:** negative tests of E0906; the units of work of the examples carry subjects; the VM determinism tests pass.

### P3. Memory and conformance tests

- `engine/memory`: keys in memory, ciphertext in rows, events and the outbox, `subjects`, `forget` as in §7; `forget.go` removed.
- `engine/conformance`: a model `testdata/pii` with every kind of place (a `self` field, `personal(path)`, a value object, an event argument, a synchronous and an asynchronous projection, a timer, an external request, a notification); the suite `RunPersonal`: plain text is not found in the raw data (the adapter provides a dump function), search and uniqueness by a blind index, `forget` releases a value, signing up again gets a new key version, a race of `forget` and a write gives a conflict.
- **Done when:** `RunPersonal` and the earlier suites pass on memory; `reasonr test` of the examples passes.

### P4. PostgreSQL

- `rt_tenant_keys`, `rt_subject_keys`, the columns `subjects text[]` with a GIN index, blind `bytea` columns; `for share` in commit; `forget` as in §7.1; `rt_forgotten` removed.
- PostgreSQL asynchronous projections and event journal; `DropTenant` deletes the keys.
- **Done when:** the conformance suites, the scenario tests in the kernel and over HTTP pass on PostgreSQL.

### P5. MongoDB

- The same key collections, a `_subj` field with a multikey index, touching the key document (§7.3).
- **Done when:** the same on MongoDB.

### P6. The effect stream and the edges

- Decrypting external requests on sending and the rule of decision 11; a bus without personal arguments; timers with a forgotten subject; responses by key; the `forget` log; masking logs and traces; cache eviction over the hub; the `vault` KMS.
- **Done when:** the failure tests (forgotten before a request is sent, before a timer fires, a command retry after `forget`) pass on the three stores.

### P7. Migrations, operations, load, documentation

- The migrations of §11, `reasonrd pii rotate|replay`, C0007–C0009, a load test with encryption, the RFC and `AGENTS.md`.
- **Done when:** the criteria of [§16](#16-acceptance-criteria).

---

## 11. Migrations and operations

| Situation | What the engine does |
|---|---|
| A field or an event parameter became `personal` | `reasonrd migrate` encrypts the values in rows in batches and builds the blind columns; each batch is its own transaction, the progress is in `rt_schema_versions`. Events already written to the outbox and the journal stay in plain text — a limitation (decision 52 of the progress: there is no `reseal-events` command) |
| A field stopped being `personal` | Decryption; the values of forgotten subjects become none; the blind column is replaced by an ordinary one |
| An index on a personal field is added | The blind column and the index are built from the decrypted values; duplicates stop the migration, as in RFC §32 |
| The first start on data in plain text | Like "a field became `personal`" for every personal field (decision 17) |
| Master key rotation | `reasonrd pii rotate`: rewrap the tenant keys with the new KMS key |
| A restore from a backup | `reasonrd pii replay --since <backup time>`: destroy the keys of the subjects from the `forget` log and erase their rows |
| Deleting a tenant | Delete the tenant key and the rows; without the tenant key the data is unreadable in backups too, where the key is also gone after the log is replayed |

---

## 12. Testing

| Kind | What it checks | How |
|---|---|---|
| The library | Encryption, AAD, versions, blind indexes, the cache | Table tests, vectors, fuzzing |
| Conformance | Stores with personal data behave the same | `RunPersonal` on memory, PostgreSQL, MongoDB |
| No plain text | Marker values (`+7999…`, `pii-marker-…`) are not found in a dump of the tables and collections, the outbox, the journal, timers, responses by key, logs | A store dump in the adapter tests, log capture |
| Scenarios | The model's logic after `forget` | New tests in taxi and bank: after `Forget` the phone cannot be read, another client takes the released number, signing up again, `forget` with an error in the same command deletes nothing; `reasonr test --engine memory` and `--engine http` (memory, PostgreSQL, MongoDB) with the same report |
| Failures | Races and late effects | Concurrent `forget` and a write of one subject on several kernels; forgotten before a request is sent and before a timer fires |
| Migrations | §11 | Two versions of the model on one database: a field became and stopped being `personal` |
| Load | [§13](#13-performance) | `TestLoad` |

---

## 13. Performance

The load scenario already writes personal data: `SignUp` with a phone and the unique index `byPhone` is one of the five commands of the loop. The goals with the same settings as in the engine plan's progress:

| Store | Now (without encryption) | Goal |
|---|---|---|
| Memory over HTTP | 25,300 commands/s | a drop of no more than 10 % |
| PostgreSQL | 2,400 commands/s | a drop of no more than 5 % |
| MongoDB | 1,300 commands/s | a drop of no more than 5 % |

The cost per command with personal data: a key cache hit, AES-GCM per value (a few microseconds or less, measured in P1), an HMAC per blind index, one extra `for share` query in the transaction. A subject's first write — one more short transaction that creates the key.

---

## 14. Schedule and milestones

Estimates for two engineers, in working weeks, ±30 %.

| Milestone | Stages | Duration | Result |
|---|---|---|---|
| **K1** | P1 | 1.5 wk | The library, the `memory` and `local` KMS |
| **K2** | P2, P3 | 2 wk | The compiler and the VM; memory with encryption, `RunPersonal`; `reasonr test` on memory |
| **K3** | P4, P5 | 2 wk | PostgreSQL and MongoDB, tests over HTTP |
| **K4** | P6 | 1.5 wk | The effect stream, logs, Vault |
| **K5** | P7 | 1 wk | Migrations, operations, load, documentation |

**Total:** about 8 weeks. End to end before complete: at the end of K2 the taxi scenarios with `forget` pass on memory with real keys.

---

## 15. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| A cryptography bug | Medium | Only standard primitives, vectors and fuzzing, a separate review of §4, AAD over everything that defines a value's place |
| Plain text left in a forgotten place (a new kind of effect, a log) | Medium | The test "the marker is not found" over every store and log; the table of §5 is a review checklist for every new port |
| Losing the master key or the tenant keys loses all personal data | Low | A KMS with redundancy; `local` only for development and closed environments, a warning in the log when started without `--test` |
| The key version check in commit lowers throughput | Medium | One query for all the subjects of a unit of work; batching the commit queries (the engine plan's backlog); a measurement in P7 |
| A race of `forget` with a write in MongoDB without read locks | Medium | Touching the key document (§7.3), a race test in `RunPersonal` |
| Decrypting every row of a page slows search | Medium | The page's keys in one query; AEADs in the cache; decrypting only the result fields |
| A backup without replaying the log brings the forgotten back | Medium | The log outside the main database, C0009, a restore procedure in the documentation |

---

## 16. Acceptance criteria

1. Personal values are not found in plain text in any store, the outbox, the journal, timers, responses by key and logs — tests on memory, PostgreSQL and MongoDB.
2. `forget` destroys the key and erases the data in the transaction of its unit of work; search and uniqueness by blind indexes work and are released after `forget`; a race with a concurrent write leaves no data under the old key.
3. `reasonr test --engine memory` and `--engine http` (memory, PostgreSQL, MongoDB) pass the taxi and bank tests, including the new crypto-shredding tests, with the same report.
4. The `memory`, `local`, `vault` KMS; `reasonrd pii rotate|replay`; C0007–C0009.
5. A load test with encryption; the goals of §13 are met or the deviations are described in the progress.
6. The RFC is updated (§17); `AGENTS.md` and the README describe `crypto`, `forget`, restoring from a backup.

---

## 17. What changes in the RFC

After the decisions of §2 are approved:

- **§20.2:** the rule E0906 (a projection operation sets the subject field together with the personal field); the subject of values in external requests, notifications and errors — by their source; the limitations of [§3](#3-threat-model).
- **§20.3:** after `forget` a subject may get data again under a new key (decision 7); `forget` releases unique values and erases the fields in rows right away; backups — through the `forget` log (decision 13); personal arguments are not published to the bus (decision 11).
- **§30.3:** `Subjects` on `ExternalRequest`, `Notification`, `BusinessError`.
- **§31:** the row "Personal data" — the key hierarchy and the KMS adapters; §31.1 — the `crypto` section; §31.2 — `rt_tenant_keys`, the version in `rt_subject_keys`, the `subjects` column, blind columns.
- **§32:** "a field became `personal`" — events in the outbox and the journal stay in plain text (a limitation, decision 52 of the progress); "a field stopped being `personal`".
- **§33:** `reasonrd pii rotate|replay`.
- **§38:** stage 4 — a reference to this plan.
