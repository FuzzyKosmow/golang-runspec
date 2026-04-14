# Integration Tests

The following test files are gated behind the `integration` build tag:

- `engine/engine_production_test.go` — runs engine resolution against real
  production plan fixtures (RX, TX, OperStatus, Equipment, etc. across device
  types). Depends on `SampleExecutionPlan-StoredInMongo/tests/fixtures/*.json`.
- `engine/engine_v2_test.go` — engine v2 fixture-driven tests. Same fixture
  directory as above.
- `orchestrator/integration_test.go` — loads every plan from `all.json` and
  drives the full orchestrator pipeline through a tracking runner. Catches
  regressions around double execution, missing post-processing, wrong OID
  generation, edge cases.
- `parser/n8n/alljson_test.go` — parses every plan in `all.json` and validates
  structural invariants.

## Why they're gated

These tests depend on fixture files that live OUTSIDE this module — specifically
in `SampleExecutionPlan-StoredInMongo/` in the consuming project. They are kept
in this repo so they travel with the code they verify, but are excluded from
`go test ./...` so the default build works from a fresh clone without fixture
setup.

## When to run

Run them on any significant package update — version bump of dependencies,
refactor of orchestrator or engine, change to guard/chain evaluation, new plan
type added. They are the regression safety net copied from the origin
(instant-scan-api) extraction.

## How to run

From the `golang-plan-runner-package` repo root, you need the fixture directory
alongside the module:

```
<workspace>/
├── golang-plan-runner-package/                  this module
└── SampleExecutionPlan-StoredInMongo/           fixtures
    ├── plans/all.json
    └── tests/fixtures/*.json
```

Then:

```
go test -tags=integration ./...
```

Hardcoded paths in the test files use `../../../../SampleExecutionPlan-StoredInMongo`
which assumes this layout. If your layout differs, either symlink the fixture
directory or patch the path constants (`testPlanDir` in `engine_v2_test.go`,
`allJSONPath` in `orchestrator/integration_test.go` and `parser/n8n/alljson_test.go`).

## Default tests

`go test ./...` (no tag) runs only the unit tests — they are self-contained
and require no external fixtures.
