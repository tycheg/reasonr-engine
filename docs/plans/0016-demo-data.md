# Plan: generating test data for demos, and a time offset

| | |
|---|---|
| **Basis** | [Roadmap](https://github.com/tycheg/reasonr-compiler/blob/main/docs/roadmap.md), item 8 |
| **Status** | Implemented 2026-11-13 (§7; the deviations are there too). Done |
| **Branch** | `demo-data` |
| **Note** | Paths and RFC section numbers are those of the time (the repository `tycheg/reasonr` before the split) |

---

## 1. Goal

Fill a tenant quickly with plausible data: customers, drivers, orders in different statuses, a month of history. The data is created **only by public commands**, as in acceptance tests. So it is consistent: events, projections, uniqueness, personal data, timers.

Today time in tests works like this:

- every test has a virtual clock that starts at 2026-01-01T00:00:00Z;
- `time passes 20s` moves it forward, and timers whose time has come fire inside the step;
- through the API the clock belongs to the test's ephemeral tenant, and the test API moves it.

Time is tied to the tenant, and the start to an invented date.

## 2. Time: an offset from now

This decision is shared by acceptance tests, demos and load ([plan 0017](0017-load-tests.md)).

| # | Decision |
|---|---|
| 1 | **A scenario runs on the current time with an offset.** `time offset -30d` or `time offset +20s` moves the scenario's clock relative to its current position. The shift holds until it is changed. Without `time offset` a scenario runs in real time. A test no longer starts at 2026-01-01 |
| 2 | **The offset belongs to the scenario, not the tenant.** The runner passes it in every request with the header `Reasonr-Time-Offset: -30d`. For the unit of work of this request the engine takes `now` = the current time + the offset. A tenant has no clock: two scenarios in one tenant each live in their own time. The `memory` driver passes the offset the same way, in the call |
| 3 | **Timers.** A timer remembers the offset of the unit of work that scheduled it. It fires when the current time + its offset reach its time: a 60-second timer scheduled "30 days ago" fires in 60 real seconds. A forward shift (`time offset +60s`) is a test API request with the offset: the engine runs the tenant's timers whose time has come by the scenario's clock right away and returns their errors, notifications and external requests, as `time passes` does now |
| 4 | **Only for tests and demos.** The engine accepts the header in test mode and for tenants with `demo: true` in `engine.yaml`. Otherwise — an error 400: time is not shifted in production |
| 5 | **`time passes d` is replaced by `time offset +d`.** `reasonr fmt` converts the examples' tests. Tests that would compare absolute times become impossible — the examples have none. Times are compared with the scenario's `now` |

## 3. Demo: a scenario in the test syntax

| # | Decision |
|---|---|
| 1 | **A demo is a `test` in a `*.demo.rr` file** with a single `given` section. The steps are public commands by an actor or a role, as in tests, plus `time offset`. There are no checks. There is no new construct: the file suffix tells the runner that this is filling, not checking |
| 2 | **Loops and variables in tests:** `let`, assignment, `for`, `while`, `if` are allowed in the sections of every kind of test. Today tests have only steps |
| 3 | **The `faker` library** — in tests, demos and load, but not in the model (an error). It is deterministic by `--seed`; by default the seed comes from the scenario's name. The language of the values — by `--lang`. Functions: `faker.firstName()`, `faker.lastName()`, `faker.name()`, `faker.phone()`, `faker.email()`, `faker.address()`, `faker.company()`, `faker.text(words)`, `faker.int(a, b)`, `faker.decimal(a, b)`, `faker.float(a, b)`, `faker.chance(p)`, `faker.pick(list)`, `faker.date(from, to)`, `faker.pattern("A###AA77")` (`#` — a digit, `?` — a letter). `phone` and `email` are unique within a run |
| 4 | **Running — `reasonrd demo load "<scenario>" --tenant demo [--config engine.yaml \| --url address] [--seed 1]`** through the API, like the `api` driver, but in an ordinary tenant with `demo: true`. Volume — `--repeat 8 --parallel 8`: the scenario runs 8 times in parallel, each time with its own seed. `reasonrd demo list` — the model's scenarios. In `reasonrd dev` — choosing a scenario in the test mode sign-in window |
| 5 | **External systems** of a tenant with `demo: true` are fakes. The fake cascade finishes inside the step, as in tests |
| 6 | **A check in `reasonrd test`:** every demo scenario runs in memory. A step error is a failure, like a `given` error |

## 4. Example

```rr
# contexts/Ordering/moscow.demo.rr
let center = GeoPoint(lat: 55.75, lon: 37.62)

test "A month of rides in Moscow"
  given
    time offset -30d
    for i in range(50)
      let d = new Driver.Id
      [d].Register(driverName: faker.name(), driverCar: Car(plate: faker.pattern("?###??77"), model: faker.pick(["Kia Rio", "Lada Vesta"]), carClass: economy)) as admin
      [d].GoOnline() as d
    for day in range(30)
      for k in range(300)                         # 300 orders over 10 hours
        let c = new Customer.Id
        [c].SignUp(phoneNumber: faker.phone()) as c
        [c].OrderTaxi(orderId: new Order.Id, orderRoute: Route(from: center, to: center, distanceKm: faker.int(1, 30)), requestedClass: economy) as c
        time offset +2m
      time offset +14h                            # the rest of the day
```

```sh
reasonrd demo load "A month of rides in Moscow" --tenant demo --config engine.yaml --repeat 4 --parallel 4
```

## 5. Questions to the author

| Question | Proposal |
|---|---|
| `time offset` — a shift from the current offset (cumulative) or setting the offset from real time? | Cumulative: `time offset +20s` is the same as `time passes 20s`; `-30d` at the start goes into the past |
| A demo — a `test` in `*.demo.rr` or its own word `demo "…"`? | `test` and a suffix: one syntax, a different runner |
| Repeating steps N times — `for i in range(n)` (a new standard library function) or `while` with a counter? | `range(n)`: shorter, and without it the example above would have to use `while` |
| Is importing ready data (CSV) needed? | Postpone |

## 6. Stages

| Stage | Result |
|---|---|
| **D1. The time offset** | `time offset`, the header `Reasonr-Time-Offset`, the offset on timers, the test API, the restriction to test mode and `demo: true`; converting `time passes`, RFC §27, AGENTS.md |
| **D2. The scenario language** | Loops and variables in tests, `faker`, `range(n)`, `*.demo.rr`, the check in `reasonrd test` |
| **D3. Loading** | `reasonrd demo load\|list`, `--repeat`/`--parallel`, a `demo: true` tenant with fakes, the choice in `reasonrd dev` |
| **D4. The example and documents** | The taxi and bank demos, the README |

The acceptance criterion:
- the taxi and bank tests pass on `memory`, `api`, `web` with `time offset` on the current time;
- `reasonrd demo load "A month of rides in Moscow" --tenant demo` on PostgreSQL fills taxi, and the UI shows orders in every status with a month of history;
- the offset header on a tenant without `demo: true` is rejected.

## 7. Progress

| Stage | Status | What was done |
|---|---|---|
| D1. The time offset | Done | `time offset ±d` (parsing also accepts the old `time passes d`, `reasonr fmt` writes `time offset +d`; every model, test, the RFC and the explanations are converted). Tests run on the current time: the clock of the in-memory engine and of an ephemeral tenant starts at the moment of the run; a negative shift moves the clock back without timers. The header `Reasonr-Time-Offset` (`port.ParseOffset`: `-30d`, `+20s`, `-1h30m`) → the offset in the request context → the unit of work's `now` = the clock + the offset, `port.Unit.Offset`. A timer is stored by its real moment (`fire_at` = the scenario moment − the offset) and its offset (`time_offset` in PostgreSQL — a new column with `default 0`, a field in MongoDB, a field in memory); `LeaseRequest.Scenario` — leasing by the scenario's clock; `Fire` runs the command in the timer's clock, new timers inherit the offset; a retry after a failure — by the real clock. The conformance check `TimerOffset` on memory, PostgreSQL (with RLS) and MongoDB. The header — in test mode and for `demo: true` tenants, otherwise 400 (`httpapi.Options.TimeOffset`, the test `TestTimeOffset`) |
| D2. The scenario language | Done | In `given` — `let`, assignment, `for`, `while`, `if` with steps in blocks (`ast.TestStmt`; in a block a call is a `TestCall`, a shift a `TimeOffset`); in `when` — `let` before the action. `range(n)` and `faker.*` (the 15 functions of the plan) are built-in bytecode functions; in the model — E0222. faker is deterministic: PCG from the test's name and `--seed`, the language from `--lang` (later removed: English data only), `phone` and `email` unique within a run. `*.demo.rr` are test files with a `Demo` flag; `when`/`then` in a demo — E0805; they do not get into the production model; `reasonrd test` runs demos like tests |
| D3. Loading | Partly done | `reasonrd demo list`, `reasonrd demo load "<scenario>" --tenant demo (--config \| --url) [--extension] [--seed] [--repeat] [--parallel] [--lang]`: the `api` driver in demo mode (`testrun.NewDemoDriver`) — commands and queries through `/api` with test sign-in and the offset header, a forward shift — `POST /_test/{tenant}/clock` by the scenario's clock, numbers — `/_test/{tenant}/ids`. `demo: true` tenants (`tenants.<name>.demo` in `engine.yaml`): the production server accepts test sign-in and the offset header for them and serves these two `/_test` routes (`testapi.DemoHandler`). Checked on PostgreSQL: "A week of rides in Moscow" — 84 orders over a week in the statuses paid, cancelled, assigned, searching in 1 s; the test `TestDemo` (memory, `--repeat 3 --parallel 3`, a refusal for a tenant without `demo: true`) |
| D4. The example and documents | Done | `examples/taxi/.../moscow.demo.rr`, `examples/bank/.../bank.demo.rr`; RFC §27.1, §27.5, §33; AGENTS.md (the section "Demo data"), the README |

**Deviations from the plan.**
- Tests on the `api` and `web` drivers still move the ephemeral tenant's clock, not the header: a test has its own tenant, so the tenant's clock is the scenario's clock, and the observed behavior is the same. The demo driver sends the header; the server accepts it in test mode too.
- Fakes are not started for a demo tenant: the `fake` transport is still only for `reasonrd --test`, and a fake command in a demo scenario is an error with a hint. The scenario calls the external systems' responses itself with their roles (both examples are written this way). Decision 3.5 — for the author's decision: allowing `fake` for demo tenants means fakes in a production process.
- There is no scenario choice in the sign-in window of `reasonrd dev` (decision 3.4).
- Loops and `if` — only in `given` (and `let` before the `when` action): `when` has one action, otherwise it is unclear whose errors `then` compares.
- `range` and `faker` — only in tests and demos (E0222), not in the whole standard library.
- ACL reactions delivered asynchronously (a bounded context in another process, plan 0011 B6) do not get the offset: their timers run by real time.
