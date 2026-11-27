# AGENTS.md — how an AI agent runs a ReasonR model on the engine

The language, the work loop with `reasonr check` and the JSON format of diagnostics are in the compiler's [AGENTS.md](https://github.com/tycheg/reasonr-compiler/blob/main/AGENTS.md). This file is about running a model: acceptance tests, the HTTP API, MCP and tests through the UI.

## Acceptance tests

`reasonrd test --format json <module>` (in a project with `reasonr.yaml` — `reasonr test --format json`: on the project's engine) runs the acceptance tests (`*.test.rr`): the base model and every extension, `--extension <name>` — one model. A failure is either a mistake in the model logic or a wrong expectation of the test: read `message`, and if needed rerun that test with `-v` (`reasonrd test -v <file>:<line>` — `file` and `line` from the report) to see the steps, errors, notifications and external requests of each step. While you edit the model, run only the affected tests: `reasonrd test --affected --base origin/main <module>`; before a commit — a full run.

## The JSON format of tests (version 1)

`reasonrd test --format json` prints one object:

```json
{
  "version": 1,
  "models": [
    {
      "module": "examples/taxi/src",
      "extension": "acme",
      "passed": 5,
      "failed": 1,
      "tests": [
        {
          "name": "The car plate is unique",
          "file": "examples/taxi/src/contexts/Ordering/order.test.rr",
          "line": 58,
          "passed": false,
          "failures": [
            {
              "kind": "missingError",
              "section": "then",
              "file": "examples/taxi/src/contexts/Ordering/order.test.rr",
              "line": 66,
              "step": "error plateTaken",
              "expected": "plateTaken",
              "message": "expected error plateTaken, got: none"
            }
          ]
        }
      ]
    }
  ]
}
```

| Field | Description |
|---|---|
| `models` | Runs by model: the module and the extension (the base model has no `extension`), the number of passed and failed tests |
| `tests` | Tests in declaration order: the name, the file and the line of the `test` declaration, `passed`, `durationMs` — the time of all runs of the test on all drivers; with `-v` — also `steps` |
| `failures` | The test's failures. `file` and `line` are the line of the step or check (`then`), and for an unexpected error — the line of the `when` step; `section` is `given`, `when` or `then`; `step` is the step or check as written in the language, with identifiers as test variable names; `message` is the message |
| `kind` | `step` — a `given` step or a query in `then` returned errors (`actual`); `fault` — an execution fault (overflow, division by zero, a limit); `ui` — `--driver web`: the action is not available in the UI (no place, the place is hidden by `if`, no row), the reason is in `message`; `missingError` — the expected error is not among the errors of `when`; `unexpectedError` — an error of `when` is not listed in `then`; `missingNotification`, `missingRequest` — the expected notification or external request is not among those sent (`actual`); `mismatch` — the field `field` of the result equals `actual[0]`, not `expected`; `noResult` — the query returned no value for `matches`; `fake` — a command a fake called returned errors |
| `steps` | With `-v`: the `section`, the `text` of the step and its outcome — `errors`, `notifications` (with the recipient `to`), `requests` |
| `run` | The test run number (from 1) on a failure, if the test ran several times: on several drivers (`--driver api,web`) or through several web places; absent in the first run |
| `driver` | The run's driver on a failure: `api`, `web`; absent for `memory` |
| `uncovered` | `--driver web --coverage`: places of the model's apps no step went through |

The rules of `then` checks (RFC §27.1): the errors of the `when` step are compared with the listed ones exactly, as a set; notifications and external requests — "contains"; arguments and `matches` fields — only the written ones. A notification without `to` in `then` matches any recipient. Step arguments are as for an API client: an omitted parameter gets its default, `PageRequest` — `size 20`. `matches` compares only value objects: the result of a page query cannot be checked that way. Each test runs on an empty engine at the current time: `time offset +20s` moves the clock forward (the timers that come due fire), `time offset -30d` moves it back; tests do not compare dates, time is relative to `now`. `given` allows `let`, `for`, `while`, `if` and `range(n)`; `faker.name()`, `faker.phone()`… give plausible values, deterministic by the test name and `--seed` (an error in the model, E0222).

Exit codes of `reasonrd test`: 0 — every test passed, 1 — failures or model errors (the diagnostics go to stderr; fix them with `reasonr check`), 2 — a fault.

## Checking through the HTTP API

`reasonrd dev <module>` serves the model's API and UI in memory on `:8080` in test mode (development only): the data tenants are `default` with the base model and one per extension, external systems run on fakes, `*.rr` changes are picked up (model errors go to the log, the previous program keeps working). The same without watching — `reasonrd --model <module> --test`. The caller is set with the header `Authorization: Test actor=<Actor>:<id>` or `Test role=<role>` (`tenant=<tenant>` — the data tenant). A command is `POST /api/{Aggregate}/{id}/{Command}` with the arguments in a JSON body, a create command is `POST /api/{Aggregate}/{Command}` without an id (the issued `id` is in the response), a query is `GET /api/{Type}/{Query}?argument=…` (`PageRequest` — the parameters `size` and `cursor`). Names in paths and headers are full (`Ordering.Order`, as in `/openapi.json`) or short if the model has only one such name (`Order`); roles are shared by the project (`admin`). Responses: 200 `{"status":"accepted","position":…,"notifications":[…]}`; 422, 403, 400 — `{"status":"rejected","errors":[{"name":…,"args":{…}}]}` (`invalidArgument` has `path` and `rule`); 500 — `incident`, the details are in the server log. Values are canonical JSON (RFC §31.3), the API description is `GET /openapi.json`.

The model is available to agents over MCP (RFC §31.3.2): `POST /mcp` in test mode, locally — `reasonrd mcp --stdio --as actor=Customer:<id> <module>`; the tools are the public commands and queries available to the caller, and tests go through them with `--driver mcp`. Acceptance tests through the API — `reasonrd test --driver api <module>`: the engine starts a test mode server itself (`--url http://localhost:8080` — one already running), each test runs in its own ephemeral tenant, and the report is the same as for `--driver memory`. To make the queries of an ordinary tenant run on fakes, set `integrations: { default: { type: fake } }` in `engine.yaml` and create a fake with the command `POST /_test/default/fakes/{System}/{Command}` with the arguments in the body, for example `Matching/Setup`.

## Tests through the UI

`reasonrd test --driver web <module>` runs the acceptance tests in a browser against a test mode server (without `--url` the engine starts it itself). `--driver api,web` runs each test through the API and through the UI places. The driver is in `reasonrd` (the web repository's `driver` package, linked by the distribution); the browser — `reasonrd web install`.

- `given`, `time passes` and the answers of external systems go through the test API. `when` steps and `then` queries go through the places of the caller's apps (RFC §22.10): a screen with parameters from the step arguments, a list row, a form, inputs, submit.
- If a step has several places, the test runs k times; failures from the second run on carry `run`.
- A `ui` failure means the place is hidden by `if`, the caller does not have it, or the row was not found. A `when` step from which the test expects errors passes when the place is hidden: the UI rightly did not allow the action.
- A step error the server returned but the UI did not show with the label `error:<name>` is a driver fault (code 2).
- `--coverage` prints the places without steps: add a test that goes through the place.
- `--place <place>` runs the steps of that method only through that place.
- `--artifacts <dir>` saves screenshots of failures, `-v` prints the steps.
