# Plan: performance tests in ReasonR

| | |
|---|---|
| **Basis** | [Roadmap](https://github.com/tycheg/reasonr-compiler/blob/main/docs/roadmap.md), item 9 |
| **Status** | L1, L2 — done; L3, L4 — partly (2026-11-13, §8) |
| **Branch** | `perf` |

---

## 1. Goal

Performance tests answer four questions:

1. **How fast is the system?** Latencies and their distribution, throughput.
2. **What load does it hold?** The largest sustained load, the number of concurrent users, scalability.
3. **What happens under overload?** Bottlenecks, errors, resource saturation, recovery.
4. **Is it getting better or worse?** Regressions between commits, releases and configurations.

Today there is only the Go test `TestLoad` of the taxi (`engine/cmd/reasonrd/server/load_test.go`): it answers only the first question.

## 2. Decisions (proposal)

| # | Decision |
|---|---|
| 1 | **A performance test is a `test` in a `*.perf.rr` file**, with the same sections as an acceptance test. No new constructs; the suffix selects the runner |
| 2 | **`given` fills the system**, as in acceptance tests: steps, loops, `faker`, `time offset` (RFC §27.5). It runs once before the load |
| 3 | **`when` is one user's path** of several steps: commands and queries. The runner runs it **in parallel** with many virtual users at the rate of the profile (decision 5). Each run has its own `let`, `new` and `faker` |
| 4 | **`then` is ordinary boolean expressions over the value `stats`**, without special syntax per metric: `stats.p95 < 100ms`, `stats.of(Customer.OrderTaxi).p99 < 300ms`, `stats.errors == 0`. `stats` is a value object of the standard library (§3). A regression is the same expression with `baseline`, the statistics of the base run: `stats.p95 <= baseline.p95 * 1.1`. Without a base such lines are skipped |
| 5 | **The load profile is not in the language but in runner flags**: `reasonrd perf <kind> [--rate 300/s] [--duration 15m] [--ramp 2m]`. Each kind has sensible defaults (§4). The load is an open model: paths start on a schedule, not in a "closed loop", so latencies are not hidden |
| 6 | **The executor** starts a test mode server on the `--config` adapters, like `--driver api`, or goes to `--url`. External systems are fakes; a fake's delay is in `engine.yaml`: `integrations: { Matching: { type: fake, delay: 50ms..200ms } }`. The report is text and JSON (`--format json`), which is also the base for `--baseline`. Exit code 1 if a `then` expression is false |
| 7 | **Microbenchmarks:** `reasonrd test --bench` — the `when` step of each acceptance test in memory, N repeats: ns/op, allocs/op, comparison with `--baseline`. The platform — Go benchmarks (`make bench`) |
| 8 | **Distributed load** — generators on workers ([plan 0018](0018-test-workers.md)), when one generator is not enough |
| 9 | **`TestLoad` is deleted**, replaced by the taxi's `peak.perf.rr` |

## 3. What `stats` has

| Field | What |
|---|---|
| `p50`, `p90`, `p95`, `p99`, `max` | Latencies of all calls (`duration`) |
| `rate` | Completed paths per second |
| `calls`, `errors`, `rejected` | The number of calls; faults (5xx, timeouts, incidents); business errors |
| `of(Method)` | The same for one method: `stats.of(Order.Card).p99` |
| `lag.projections`, `lag.timers`, `lag.requests` | The lag of asynchronous projections, timer firing, external request delivery (p99) |
| `cpu`, `memory`, `dbWait` | Engine resources and the wait for a database connection — from Prometheus metrics (the maximum over the run) |
| `recovery` | Stress and spike: how soon latencies returned to normal after the load dropped |

## 4. Test kinds

The same `*.perf.rr` runs as different kinds. The kind sets the profile and what to do with `then`.

| Kind | Question | How | When |
|---|---|---|---|
| Microbenchmark | 1, 4 | `reasonrd test --bench`, `make bench` | Every MR, locally |
| Smoke | 1 | `reasonrd perf smoke`: 20/s, 1 minute, `then` at the end | Every MR |
| Load | 1, 2 | `reasonrd perf load --rate 300/s`: a ramp and 15 minutes of the expected load, `then` at the end | CI, nightly |
| Stress | 2, 3 | `reasonrd perf stress`: the load grows in steps until failure. The report — what broke first (latencies, errors, lag, a resource) and `recovery` | Weekly, before a release |
| Spike | 3 | `reasonrd perf spike --rate 100/s --spike 1500/s`: a jump and a drop, errors during the jump, `recovery` | Periodically |
| Soak | 3, 4 | `reasonrd perf soak --duration 8h`: `then` is checked in every 10-minute window, so slow degradation (memory, lag) is a failure | Nightly, periodically |
| Capacity | 2 | `reasonrd perf capacity`: the largest rate at which `then` holds (a search by steps) | Before sizing production |
| Scalability | 2 | `reasonrd perf scalability --config a.yaml --config b.yaml` or `--replicas 1,2,4`: capacity on each configuration | Before architecture decisions |
| Regression | 4 | Any kind with `--baseline report.json`: the `then` lines with `baseline` | Always |

## 5. Example

```rr
# contexts/Ordering/peak.perf.rr
let center = GeoPoint(lat: 55.75, lon: 37.62)
let ride = Route(from: center, to: center, distanceKm: 5)

test "Peak hour"
  given
    for i in range(500)
      let d = new Driver.Id
      [d].Register(driverName: faker.name(), driverCar: Car(plate: faker.pattern("?###??77"), model: "Kia Rio", carClass: economy)) as admin
      [d].GoOnline() as d
  when
    let c = new Customer.Id
    let o = new Order.Id
    [c].SignUp(phoneNumber: faker.phone()) as c
    [c].OrderTaxi(orderId: o, orderRoute: ride, requestedClass: economy) as c
    Order.Card(orderId: o) as c
  then
    stats.p95 < 100ms
    stats.of(Customer.OrderTaxi).p99 < 300ms
    stats.errors == 0
    stats.lag.projections < 1s
    stats.p95 <= baseline.p95 * 1.1
```

```sh
reasonrd perf smoke --config engine.yaml                                      # every MR
reasonrd perf load --rate 300/s --config engine.yaml --baseline main.json      # nightly, with regression
reasonrd perf capacity --config engine.yaml                                   # "holds 840/s"
```

## 6. Questions to the author

| Question | Proposal |
|---|---|
| `then` as boolean expressions over `stats` — does it fit? | Yes: one construct for every metric; new metrics are new `stats` fields, not syntax |
| The load profile — runner flags or in the test? | Flags: one test runs as different kinds |
| How to refer to a method in `stats.of(…)` — by name (`Customer.OrderTaxi`) or a string? | By name: the compiler checks that the method exists |
| A mix of paths: several `*.perf.rr` at once with shares? | `reasonrd perf load a b --mix 70,30` — later, if needed |

## 7. Stages

| Stage | Result |
|---|---|
| **L1. Language** | `*.perf.rr`, several steps in `when`, `then` as boolean expressions, `stats` and `baseline` in the standard library |
| **L2. Executor** | `reasonrd perf smoke\|load`: the open model, HDR histograms, a JSON report, `--baseline`, fake delays |
| **L3. Overload and capacity** | `stress`, `spike`, `soak`, `capacity`, `scalability`; lags and resources from metrics, `recovery` |
| **L4. Benchmarks, migration and docs** | `reasonrd test --bench`, `make bench`, the taxi's `peak.perf.rr` instead of `TestLoad`, a CI example, README, RFC |

Done when:
- `reasonrd perf load` on PostgreSQL checks the taxi's `then`, and a false expression gives code 1;
- `capacity` finds the taxi's limit;
- `--baseline` catches an artificial 20 % slowdown of a command;
- `TestLoad` is deleted.

## 8. Progress

| Stage | Status | What is done |
|---|---|---|
| L1. Language | Done | `*.perf.rr` files (the `Perf` flag; they are not part of the working model). The `when` of a performance test is a path of several steps (commands, queries, `let`, `time offset`). `then` is boolean expressions (`ast.ThenExpr`): `stats` and `baseline` are the built-in value object `Stats` (`p50`…`max` — duration, `rate` — float, `calls`, `errors`, `rejected`), `stats.of(Type.method)` — by a public command or query (the compiler checks it). Each check is its own IR function with a bool result (`lower.PerfCheck`), the fields are the built-in `perf.stat` (`lower.PerfKey`); VM: `Machine.PerfCheck`, run phases `PhaseGiven` and `PhaseWhen` |
| L2. Executor | Done | `engine/perf`: given once in an ephemeral tenant of a test mode server (`--config` — in this process, `--url` — a running one), the when path on an open-model schedule with a linear ramp, a limit on concurrent paths (`--max-inflight`, above it — a fault), each `/api` call measured (`testrun.NewPerfDriver`), percentiles over all calls and by method, faults and business errors. `reasonrd perf smoke\|load`, the report as text and JSON, `--baseline` (checks with `baseline` are skipped without a base), code 1 on a false check |
| L3. Overload and capacity | Partly | `reasonrd perf capacity`: steps of ×1.5 from `--rate` while the checks hold and the generator keeps up (≥ 90 % of the set rate); on PostgreSQL the taxi holds 506 paths/s (3 calls per path; p95 6.9 ms), at 759/s p95 is 147 ms. `stress`, `spike`, `soak`, `scalability`, the lags of projections, timers and requests, resources from metrics and `recovery` are not done |
| L4. Benchmarks, migration and docs | Partly | The taxi's `peak.perf.rr`; `TestLoad` is deleted (`latencies` moved to `TestLoadRequests`). `TestPerf`: a false check gives code 1; a proxy stretching `OrderTaxi` responses by 20 % — `--baseline` catches the regression, without the slowdown the check passes. RFC §27.6, §33, AGENTS, README. `reasonrd test --bench` and the CI example are not done |

**A check on PostgreSQL** (the bench as in [plan 0015](0015-scale-10m.md)): `reasonrd perf load --rate 100/s --duration 30s --ramp 5s` — 99.7 paths/s, 9,720 calls, p50 2.1 ms, p95 4.0 ms, p99 4.8 ms, no faults, the `then` checks hold.

**Deviations from the plan.**
- A regression is written as multiplication by an integer (`stats.p95 * 10 <= baseline.p95 * 11`): in the language a duration multiplies by an integer but not by a fraction; `* 1.1` would need a new typing rule.
- Fake delays (`integrations: { Matching: { type: fake, delay: … } }`) are not done: the `peak.perf.rr` path does not wait for the answers of external systems.
- `TestLoad` also checked the delivery latency to the integration service and to Kafka; `reasonrd perf` does not have it yet (the `stats.lag` field — stage L3).
- `reasonrd test` runs a performance test like an ordinary one (given and one path), without the `then` checks.
