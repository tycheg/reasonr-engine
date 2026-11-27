# Plan: test run workers — a pull model

| | |
|---|---|
| **Basis** | [Roadmap](https://github.com/tycheg/reasonr-compiler/blob/main/docs/roadmap.md), item 10 |
| **Status** | W1, W2 — done; W3 — partly (2026-11-13, §6) |
| **Branch** | `test-workers` |

---

## 1. What exists and how the item is read

`reasonrd test` runs tests in one process: the list of tests is split between goroutines (`--parallel`), and each creates its driver (the engine in memory, an ephemeral tenant on a server, a browser). The distribution is **push**: the runner knows the executors in advance and hands tests out to them. For 100,000 tests ([plan 0015](0015-scale-10m.md)) and with the `web` driver (browsers) one machine is not enough, and splitting "by files across N machines" in CI gives unequal chunks: long tests end up together.

The item is read as: **a test queue and workers that take work themselves** (pull): as many workers as have connected are working; fast workers take more; a crashed worker does not lose tests; workers scale separately (browser ones on machines with Chromium).

## 2. Decisions (proposal)

| # | Decision |
|---|---|
| 1 | **The coordinator** — `reasonrd test --serve [--listen :7070]`: compiles the model (or accepts built bytecode), makes the jobs "test × driver × place run" and holds the queue. It also collects the report — the same as now (`--format json`), plus the worker of each result |
| 2 | **A worker** — `reasonrd test-worker --coordinator http://…:7070 [--drivers memory,api,web] [--slots 8]`: connects, gets the program (the bytecode and the UI program by hash, cached), and in a loop **takes** a job with a fitting driver, runs it, returns the result. A worker with `web` needs `reasonr-web` and a browser; with `api` — an engine server (its own, in test mode, or a shared `--url`) |
| 3 | **Job leases:** a job is handed out with a lease (say, 2 min, extended by a heartbeat); if the worker disappears, the job returns to the queue; at most 2 retries, then a `fault` failure "the worker did not return a result" |
| 4 | **The protocol** is HTTP/JSON (`POST /lease`, `POST /result`, `GET /program/{hash}`), authenticated with a shared token (`REASONR_TEST_TOKEN`); later the same protocol serves distributed load ([plan 0017](0017-load-tests.md)) |
| 5 | **One process is a special case:** `reasonrd test` without `--serve` starts the coordinator and workers in its own process, so the behavior and the report are the same; `--parallel` is the number of slots of the local worker |
| 6 | **CI:** the coordinator is a pipeline step, the workers are parallel jobs (a matrix) that finish when the queue is empty; a GitHub Actions example in the README |
| 7 | **Test impact analysis and a cache** (RFC §27.4): the coordinator queues only the affected tests; later it reuses results by hash |

## 3. Example

```sh
# the coordinator
reasonrd test --serve --listen :7070 --driver api,web --format json examples/taxi/src > report.json
# workers on three machines
reasonrd test-worker --coordinator http://ci-coord:7070 --drivers api --slots 16
reasonrd test-worker --coordinator http://ci-coord:7070 --drivers web --slots 4
reasonrd test-worker --coordinator http://ci-coord:7070 --drivers web --slots 4
```

## 4. Questions to the author

| Question | Proposal |
|---|---|
| Keep the queue in the coordinator's memory or in an external broker (Redis, Kafka)? | In the coordinator's memory: a run is short, and if the coordinator crashes the run restarts |
| Does a worker with `api` start its own engine server, or do all go to a shared one? | Its own (test mode in memory) by default; a shared one with `--url`, to check on a real database |

## 5. Stages

| Stage | Result |
|---|---|
| **W1. Coordinator and worker** | Jobs, leases, the protocol, the program cache, the report; the local mode through the same queue |
| **W2. Reliability** | Retries when a worker is lost, a `fault` failure after two retries |
| **W3. CI and docs** | A pipeline example, README, RFC §27.3; the link with `--affected` (RFC §27.4) |

Done when:
- the taxi and the bank on `--driver api,web` with a coordinator and three workers give the same report as a local run;
- a worker killed in the middle of a run loses no tests;
- on the synthetic model ([plan 0015](0015-scale-10m.md)) with 10,000 tests the run time falls almost linearly with the number of workers (8 workers — at most 1/6 of the time of one).

## 6. Progress

| Stage | Status | What is done |
|---|---|---|
| W1. Coordinator and worker | Done | `testrun.Coordinator`: jobs "a model test × drivers", an in-memory queue, a lease (2 min by default, `REASONR_TEST_LEASE`), `POST /lease`, `/heartbeat`, `/result`, the token `REASONR_TEST_TOKEN`; the report is the same as a local run's. `reasonrd test --serve [--listen]` is the coordinator with the usual flags (`--driver`, `--run`, `--affected`); `reasonrd test-worker --coordinator … [--drivers] [--slots] [--url \| --config] [--name]` is a worker: it builds the model from its own copy of the module once and checks the source hash against the job, starts its own server for the `api` and `web` drivers (with `web` the command goes to `reasonr-web test-worker`), and prepares the program and the test places once per model |
| W2. Reliability | Done | An expired lease puts the job back in the queue, after two retries — a `fault` failure "the worker did not return a result"; a late result after a retry is accepted once. A worker whose model did not build or whose server did not start exits without taking jobs. After the results the coordinator answers "no jobs" for one more second so that waiting workers finish without an error. `TestWorkers` (the taxi and the bank, `memory,api`): a coordinator and three workers give the same report as a local run; a "lost" worker with five jobs loses no tests |
| W3. CI and docs | Partly | RFC §27.3 (workers), §33, README. The GitHub Actions pipeline example is not added |

**Measurements** (one machine, 10 cores; the synthetic model of [plan 0015](0015-scale-10m.md), 1m — 18,478 tests, the `api` driver, each worker with its own server in memory): 1 worker — 456 s, 8 workers — 154 s (≈ 3×); every test passed. On 100k (1,882 tests, `memory`) — 4.7 s and 2.3 s: tests in memory take microseconds, and the time goes to jobs (≈ 2 ms per lease and result round).

**Deviations from the plan.**
- The criterion "8 workers — at most 1/6 of the time of one" is not checked: on one machine 8 workers and their 8 servers share 10 cores. A measurement on separate machines is needed.
- A worker does not get bytecode by hash but builds the program from its own copy of the module and checks the source hash: so it needs no transfer format for bytecode and dependency modules; in CI each worker has the same repository.
- The local `reasonrd test` still splits tests between goroutines directly, not through the same queue (decision 5): the behavior and the report are the same, and an extra HTTP round would cost ≈ 2 ms per test.
- A job is a test on every driver at once (as in a local run), not "test × driver × place run".
- UI place coverage (`--coverage`) is not collected through workers.
