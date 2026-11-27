# Progress: the interface ([plan 0006](0006-ui.md))

Paths are those of the time (the repository `tycheg/reasonr` before the split).

| Stage | State | Where |
|---|---|---|
| U1. The language | Done | `compiler/token`, `compiler/lexer` (`|`, `#n` in text keys), `compiler/ast` (`ui.go`, `primitive.go`, `texts.go`), `compiler/parser/ui.go`, `compiler/format/ui.go`, `compiler/std` (the catalog `std/ui.rr`), `compiler/module` (the catalog, `extend screen`), `compiler/sema` (`primitive.go`, `uicheck.go`, `uilayout.go`, `uinodes.go`, `texts.go`, `ui.go`), `compiler/diag` (E0701–E0716, E1011, W1301–W1303; explanations — `explain/e07.md`), `compiler/tools/explainui.go`, `compiler/tools/graph.go` (`--app`), `lsp/ui.go`; taxi — `examples/taxi/{ui,admin,staff}.rr` and its texts file |
| U2. The UI program | Done | `ui.go` (`reasonr.UI`), `compiler/uigen` (the program, `--ui-types`), `compiler/sema/uiauto.go` (`app … auto`), `compiler/lower/ui.go` and `runtime/vm/ui.go` (layout expressions, `Machine.Eval`), `engine/uiview` (screen descriptions and manifests), `reasonr ui eject`, `reasonr build --dump-ui --ui-types` |
| U3. The server | Done | `edge/appapi` (the manifest, descriptions, screen data, places, the app's OpenAPI, serving the runtime), `engine/port/ui.go` (`UICatalog`), `edge/httpapi` (`Handlers`, the WebSocket subprotocol, `AppendError`, `AppendNotification`), `engine/builtin` (`ingress[].ui`), `engine/check` (C0014, C0015), `edge/webui` (the built-in build), `cmd/reasonrd/server/ui.go` |
| U4. The web runtime and the catalog | Done | `web/packages/runtime` (`@reasonr/runtime`), `web/packages/std-ui` (`@reasonr/std-ui`, Ant Design 6), `web/packages/vite` (`@reasonr/vite`), `web/app` (the built-in build → `edge/webui/dist`), `web/test` (the conformance suite), `engine/check` (C0016), `make web`, `make web-check`, the CI job `web` |
| U5. `--engine web` | Done | `testrun/web` (a separate module: `Runner`, `Driver`, places, `Engine`; the taxi test in a browser and the condition mutation), `testrun/web/cmd/reasonr-web`, `testrun/command.go` (`Command`, `Engine`), `testrun/httpui.go`, `runtime/vm/test.go` (`Step.Unavailable`, `SectionDriver`, the failure kind `ui`, `Failure.Run`), `cmd/reasonr/test.go` (delegating `--engine web`, `--install`); RFC: the app `Admin` for `admin, support`, `Customer.SignUp` on the `Profile` screen; the CI job `web` installs Chromium |
| U6. Load and documentation | Done | measurements — decision 50; the README "Interface", `AGENTS.md` ("Interface", "Tests through the UI", the kinds `ui`, `run`, `uncovered`), RFC §31.1 (`ingress[].ui`), §31.4, §33, §38, §27.1, §27.3; `cmd/reasonrd/engine.example.yaml` |

## Decisions made along the way

1. **Catalog names and model types.** The primitives of `std/ui.rr` are not in the model's global namespace: a type may be named like a catalog primitive (`value Row`, `value Card` are ordinary names in models, and the examples already use them). A type and a primitive are written in different places, so there is no confusion. A screen, an app or one's own primitive with a catalog name is E0101 ("catalog names cannot be redeclared"). RFC §22.8 is extended; RFC §23 still says that model names are unique.
2. **`Card<V value>`.** The RFC had `Card<V>`, but then `Card` would take a query with a page. The `value` constraint gives the error E0702, as the plan intended (§3.5). RFC §22.7–22.8 are fixed.
3. **The default base language is `en`.** Every taxi app states its `language` explicitly: `DriverApp` and `Dispatch` did not have it.
4. **Node keys.** The first node with a key on a screen has no number, the second is `#2`. This also applies to nodes without a binding (`AdminOrders:Toolbar`, `AdminOrders:Toolbar#2`). A part is `Screen:Part`, a field line is `Screen:field`. RFC §22.11 is refined. Places are the same keys of nodes bound to methods: `Admin.AdminOrder:Order.Edit`; at the root of an app — `Admin:…`.
5. **Which parts are filled automatically.** In the RFC `RowActions` and `Columns` are both `binds Row`, but `RowActions` is "a part of primitives only". The rule: a value's fields are shown by the first part of the primitive that binds that type (children come before parts). The following parts with the same type only give a row scope. The parts `params` and `inputs` are filled only if there is something to enter. RFC §22.7 is extended.
6. **Inputs with an initial value.** A command argument from the data, a row, a form or an event is a form input with that initial value (`Arg.Input`). Example: `orderRoute` of `Order.Edit` on the order card. A query argument from the same scopes is a fixed link, not a filter. An explicit link to a data field (`newEmail: email`) allows `T?` for a parameter `T`: it is the input's initial value, and the command checks the value itself.
7. **Roles of the app's actor.** `app(Staff)` admits staff who have roles (`admin`, `support`). So a method with access by role in an actor's app does not give W1301. The warning stays for methods of other actors. By the same rule `explain` lists the methods missing from the interface.
8. **The data of the `done` event** are every argument of the command and the target. The target's name is the type in lowercase (`customer`): the form field is named the same way when the target is entered.
9. **`me` on a screen of several apps.** The target `me` is taken only if the target type is the actor of every app the screen belongs to. If only of some — E0711. The type of `me` in a screen's expressions is the common actor of all its apps.
10. **Default primitives** are looked up at three levels: the screen's apps → the module → the catalog. At one level an exact type wins over a constraint (`id`, `enum`, `value`). `default` parameters (for example, `search` of `EntityPicker`) are checked once — at the declaration. The catalog got `default`s for every simple type, list and set (display and input).
11. **Diagnostic codes.** E0701–E0716 — one per group of checks of the plan's §3.5, with a specific text in the message. W1301–W1303 — warnings. Explanations — `compiler/diag/explain/e07.md`; the former E08xx–E09xx moved to `e08.md`. E0805 and W1011 are removed.
12. **`extend screen` is done now, not in stage 7.** The RFC is the source of the examples (`go test ./examples`), and the §21 block with `extend screen MyOrder / in Actions` must compile for the tenant acme. `in Part` lines are added to the only line of that part on the screen; with several lines — E1011, with none — E1002.
13. **The screen `OrderCard` is renamed `MyOrder`** — in the RFC it clashed with the value `OrderCard` (§25.1). RFC §21 and §25.5 are fixed.
14. **`Staff.Hire`.** Without a hiring command the field `Staff.name` is never set (W1001), and the examples must check without warnings. The command is added in RFC §22.3.
15. **RFC blocks.**
    - §22.3 is split: `staff.rr` is an example file; the model additions are an illustration, and the files themselves are in §25.1–25.3.
    - The second-language texts of §22.11 are a separate block without a path: the taxi example has no translation.
    - The file headers of `ui.rr` and the texts file are exactly a path, otherwise the block does not count as an example file.
16. **Tokens.**
    - `#` right after a name and before a digit is a node number in a text key (`Order.Cancel#2`); a comment is separated by a space.
    - A single `|` is type variants; `||` and `&` are still an error with a hint.
    - A bare `new` is allowed only as an argument in a layout: the type comes from the parameter.
17. **A schema without the interface** (decision 2): `TypeView`, `TypeControl`, `ViewInfo`, `ViewItem` are removed. An old schema with the kind `view` cannot be read — there are no models outside the repository. RFC §30.5 is fixed.
18. **Substitutions in texts** — only in the texts of errors and notifications, only a parameter name. The warning about a missing translation — only for languages that have a `texts` block in the model.

### U2

19. **Layout expressions are bytecode functions of kind `ui`** (`FormatVersion` 4). These are `if` conditions, expressions in binding arguments and in primitive parameters. The function name is `ui.<screen>:<node key>.<if | argument | parameter>`. The parameters are the scope values the expression reads, in the order of first occurrence. The server calls the function through `Machine.Eval`. A constant needs no function: its value is in the UI program as canonical JSON.
20. **The UI program `reasonr.UI`** is in the root package (`ui.go`), next to `Schema` but apart from it. Types in it are written as language strings, and the model's enums and value objects are in `UI.Types`: a screen description needs no schema. A screen's nodes are numbered from 1 in traversal order, and arguments refer to data, row and form nodes by number.
21. **The compiler fills omitted parts** (`compiler/uigen`): a value's fields, filters and inputs with default primitives. Such nodes are marked `auto`. The runtime gets a ready tree and infers nothing itself. Nested fields are built to a depth of 4: a value object may refer to itself through `T?`.
22. **Screen descriptions** (`engine/uiview`) — per app, screen, language and platform. A description holds the labels in the user's language, the place of every bound node, only the types met on the screen with the labels of their values and fields, and the texts of the errors and notifications of the types of its methods. The hash is 16 hex digits of SHA-256. The app manifest holds the root (the menu), the hashes of the screen descriptions and the places. The platform does not affect the content yet.
23. **Labels** are looked up like this:
    - the texts of the user's language;
    - the texts of the base language — layout strings among them;
    - the name split into words.

    The keys — first the node's (`Screen:key`), then the model's: the binding method, the text key of a field (`OrderRow.placedAt`, `Customer.OrderTaxi.requestedClass`), the target screen of a navigation.
24. **The automatic UI** builds layout text. The text is parsed as a file `auto/<App>.rr` and then checked as if written by hand. The app's `language`, `platforms`, `default` lines are carried into the text as they are.
    - Screen names: `<App><Type>` (the menu), `<App><Type>Card` (a card), `<App><Type><Command>` (a form); on a clash with a taken name a number is added.
    - Methods are taken by strict access: the method's access names an actor or a role from `app(…)` explicitly. The assumption "an actor may have roles" (decision 7) does not fit the automatic UI — otherwise the customer app would get the administrator's methods. The taxi admin is built automatically by `app(Staff, admin, support) Admin auto`.
25. **The properties of implementations** (`--ui-types`), set by the runtime:
    - `data`, `loading`, `loadMore`, `hasMore` — a query;
    - `submit`, `busy`, `errors` — a command;
    - `value`, `onChange`, `error` — a field and an input;
    - `href`, `go` — a screen;
    - `caption` — a label, `children` and parts — nodes or row render functions, `on<Event>` — events, `rr` — labels.

    A primitive's parameters cannot have these names (E0701). Query parameters are functions that call the query on the server; `field` — row functions.

### U3

26. **The model's interface for the transport** is `port.UICatalog`: the model schema → the UI program and the interpreter's `Eval`. The executor returns the tenant's schema, the interface is found by it, so the ephemeral tenants of `--test` work without changes. `reasonrd` builds the catalog from its models. The catalog gets into the HTTP ingress through `registry.Deps.UI`.
27. **`httpapi` does not know about `appapi`.** `httpapi.Options.Handlers` are handlers built at start with the executor. `appapi` imports `httpapi` for encoding errors and notifications, not the other way round. `engine/builtin` mounts `appapi` at `/app/` and `/ui/`.
28. **Addresses.** Places are addressed by node number (`/app/Admin/AdminOrder/18`); the node key (`Order.Edit`) is accepted too. The arguments of a place query are one parameter `args` with an object in the canonical JSON. A primitive's query parameter is called as `?param=search` on the field node. The body of a place command is `{"target": …, "args": {…}}`, the response is as from the model API.
29. **`GET /app/_login`** answers without signing in: test mode or the OIDC settings. The runtime needs to know how to sign in before signing in. The manifest and screen descriptions require signing in: the model depends on the caller's tenant.
30. **Entering an app** — an actor type or a role from `app(…)`. A node is hidden if the caller has no access to the method by roles and actor type. The command itself checks `self` and field access.
31. **Screen data.**
    - Queries run along the tree, sequentially. Scope values (screen parameters, `me`, node data) are encoded into the canonical JSON and decoded as arguments — so the checks are the same as in the model API. Running independent queries in parallel is postponed until the U6 measurements.
    - A query with an input that has no default value is `pending`. A place query runs it once the user enters a value.
    - For nodes in table or list rows the `if` condition is computed per row: `rows` — node number → visibility by row. A place query answers the same way: the next page comes with its own visibility.
    - Queries inside a row (not in a card) are not part of the screen data: a place query runs them.
32. **The description cache** — by the UI program, the app, the screen, the language and the platform. An address with a stale hash answers 404: the runtime rereads the manifest.
33. **WebSocket from a browser.** Credentials are passed in a subprotocol: `reasonr, bearer.<token>`, in `--test` — `reasonr, test.<base64url of the Test credentials>`. The server picks the subprotocol `reasonr`. The `access_token` parameter is still accepted. There is no separate `/app/{App}/ws`: the runtime connects to `/api/ws`.
34. **The built-in build** is `edge/webui/dist` (`go:embed`). Until U4 it holds a stub `index.html`. An unknown path under `/ui/` returns `index.html`; the files of `assets/` are cached forever.

### U4

35. **Versions.** React 19, Ant Design 6, Vite 6 and Vitest 3. Vite 7 needs Node 20.19+, and the environment and CI have Node 20. The packages are used from their sources (`main: src/index.ts`): there is no published library yet, only the app is built.
36. **Parts and children in implementation properties** are arrays of items with a label:
    - `Item` — `{key, label, node}`;
    - `RowItem<Row>` — `{key, label, render(row, i)}` for row parts.

    `Table` takes its columns with headers from them, `Card` and `Form` the field labels. In the UI program a node has a `rows` flag: the part or the children repeat per row. The runtime need not infer this from types. `--ui-types` is updated.
37. **An input's default value in the description** is `arg.value` (for example, `wanted = searching` of `Order.List`): the runtime fills the filter with it.
38. **The runtime does not know primitive declarations.**
    - The properties are gathered from the node: the binding, the navigation, the parameters, the parts (`part` nodes), the events (`on` nodes), the children.
    - The implementation registry: the module → the catalog → the automatic UI (`Auto`).
    - Argument expressions with a function are not computed in the browser (decision 4): constants and scope values are supported.
    - A nested screen is drawn as a link for now.
39. **Labels.**
    - `place:`, `input:<place>.<path>`, `submit:`, `result:` with `data-rr-value`, `item:<place>:<row id>` (the first `…Id` field of the row), `screen:`, `error:` and `notification:` (the runtime's message area).
    - Added `open:<place>` — the button that opens a form window (`Edit`, `Confirm`).
    - The runtime gives the UI test runner `window.__reasonr.inputs[label](value)`. The runner does not fill complex widgets (a select with a search) character by character: it checks the place, the form and the submission.
40. **Signing in.** In `--test` — a "sign in as" form: the actor, the identifier (a new uuid by default), the roles, the tenant; the `Test …` header. OIDC — the authorization code flow with PKCE: discovery, sign-in, exchanging the code for a token. `me` is the token's `sub`, the session is in `sessionStorage`. The WebSocket opens with the subprotocol of decision 33.
41. **The build manifest `reasonr-ui.json`** (`{std, primitives}`) is created by the plugin `@reasonr/vite`, and the built-in build includes it too. C0016 checks the primitives of apps with the `web` platform against it. Without `ui.dir` the built-in build — the standard catalog — is checked. `reasonrd` and `reasonr engine check` pass the UI programs for the check (`check.Options.UI`).
42. **The size of the built-in build** — 348 KB gzip (the goal is ≤ 400 KB, §12), as one file. Splitting by screen — if needed after the U6 measurements.

### U5

43. **`reasonr test --engine web` is run by a separate command `reasonr-web`** from the module `testrun/web`. This is decision 12: the kernel does not depend on Playwright. `reasonr` finds it in the PATH and passes it the arguments. The flags, the run and the report are shared with `reasonr test` (`testrun.Command`, the interface `testrun.Engine`). `reasonr test --install` installs the browser (`reasonr-web install [--with-deps]`). Playwright — `github.com/mxschmitt/playwright-go`: the module v0.6401.0 from playwright-community is declared under this path.
44. **Steps.**
    - `given`, `time passes`, commands of external systems and fakes, and steps whose caller is an external system's role go through the server's test API (`HTTPDriver`, the same ephemeral tenant).
    - `when` steps and `then` queries go through the places of the apps the caller belongs to. The app's platform does not matter: mobile is checked by the same web runtime.
    - Every step runs in a new browser context with the session in `sessionStorage`. The screen opens at an address with `_after` — the position of the last unit of work.
45. **Screen parameters** are taken from the step's arguments in this order: by the place's links (`from param`), then by the target type (`Order.Id` ← the step's target), then by name. If a required parameter cannot be filled, the place is not applicable.
    - The runtime takes new identifiers (`new`) from the queue `window.__reasonr.ids`.
    - Input values are passed through `window.__reasonr.inputs`.
    - If the place is in a list row, the runner looks for the row `item:<list>:<id>` and pages through "More" (`more:<list>`) if needed.
46. **A step's result comes from the place API's response**, not from the DOM: errors, notifications, the position and the recorded external requests (`Captured`). Every error of a step must be shown by an `error:<name>` label, otherwise it is a failure.
    - A `then` query reads the node's data from the screen data response. If the step has filters different from the default values, they are entered, and the data comes from a repeated node query.
    - The query result must be shown (`result:` with `data-rr-value`).
47. **An unavailable step.**
    - The reasons: the step has no place, the place is hidden by an `if` condition, there is no row or no screen parameters.
    - A `when` step with such an outcome passes if the test expects errors from it: the UI rightly did not let the action happen. Otherwise it is a failure of kind `ui` ("the action is not available in the UI").
    - In `given` and `then` an unavailable step is always a failure.
    - If the place is shown and the server rejects the command, the test passes too: the errors are compared as usual.
48. **k runs.** k is the largest number of applicable places of a step in the first run. In the i-th run a step starts from the i-th place, round robin; if it is hidden, the next ones are taken. The failures of runs from the second on have a number (`run` in the JSON).
    - `--place` narrows to one place only the steps of its method; the rest run as usual.
    - `--navigate path` opens a screen without parameters by a menu item (`link:`).
    - `--coverage` lists the places no step went through (`uncovered` in the JSON).
    - `--artifacts` saves screenshots: on a failure and when a step found no applicable place.
49. **The check.**
    - The taxi tests (base and acme, 20 each) pass through the UI. A run takes about 6 s with `--parallel 8`.
    - If the condition of `Order.Edit` in `admin.rr` is spoiled (`status == assigned`), the test "Staff changes the route…" fails with `ui` and `mismatch`.
    - Both checks are in `TestTaxi` of the module `testrun/web`. Without Chromium the test is skipped; in CI the `web` job installs Chromium.
    - Paging through "More" is not checked on taxi: there are few rows.

### U6

50. **Measurements (§12).** Conditions: Apple M1 Max, one `reasonrd` process, the client on the same machine.

    | Metric | Goal | Result |
    |---|---|---|
    | Compiling taxi with the interface (`reasonr build --tenant acme`, the median of 30 runs) | ≤ +20 % | 14.3 ms against 14.0 ms without `ui.rr`, the `admin.rr` screens and `extend screen` (+2 %); the catalog `std/ui.rr` is included in both cases |
    | A screen description, gzip | ≤ 20 KB | at most 1.7 KB (`AdminOrder`) |
    | The manifest, gzip | ≤ 5 KB | at most 0.6 KB (`Admin`) |
    | Screen data, PostgreSQL 17, 500 requests/s, 20 s | p99 ≤ 30 ms | `Admin.AdminOrder` — p99 6.8 ms; `Taxi.MyOrder` — 8.4 ms; `Taxi.MyOrders` (a page) — 6.7 ms; no errors |
    | The built-in build, JS gzip | ≤ 400 KB | 346 KB, one file |
    | Opening a screen up to the data (Chromium, a warm cache, 60 openings) | p95 ≤ 300 ms | `AdminOrders` — p95 69 ms, `NewDriver` — 69 ms |
    | The taxi tests `--engine web`, `--parallel 4`, base and acme | ≤ 5 min | 11 s in memory, 11.5 s on PostgreSQL |

    A deviation: taxi has no card with two queries. Instead, a card with one query and a form with initial values (`AdminOrder`) and a list page were measured.
51. **The criteria of §15.**
    - The taxi tests (base 28, acme 29) pass `--engine web` in memory and on PostgreSQL; `--coverage` — every place is covered.
    - For this the tests `taxi/ui.test.rr` and `taxi/tenants/acme/order.test.rr` were added (RFC §27.3).
    - So that tests could call queries with a page, arguments in a test are treated as from an API client: a `PageRequest` may be omitted (RFC §27.1, sema).
    - OIDC sign-in is checked by the runtime test `web/test/oidc.test.ts`: the redirect to the IdP with PKCE S256 and state, exchanging the code with `code_verifier`, `me` is `sub`, a foreign state. C0015 checks the configuration. Signing in with a real identity provider was not checked.
52. **Found by tests through the UI.** Changing a query filter went out with the old values: a closure fired before rendering. The errors of a repeated node query were not shown. Both flaws are fixed, and a Vitest test calls an input without `act`, as a browser does.
