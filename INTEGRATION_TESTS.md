# Integration Tests

The following test files are gated behind the `integration` build tag:

- `engine/engine_production_test.go` — engine resolution against real production
  plan fixtures (RX, TX, OperStatus, Equipment, etc. across device types).
- `engine/engine_v2_test.go` — engine v2 fixture-driven tests.
- `orchestrator/integration_test.go` — loads every plan from `all.json` and
  drives the full orchestrator pipeline through a tracking runner. Catches
  regressions around double execution, missing post-processing, wrong OID
  generation, edge cases.
- `orchestrator/orchestrator_test.go` — smaller-scoped guard/chain scenarios
  that also load fixture plans.
- `parser/n8n/alljson_test.go` — parses every plan in `all.json` and validates
  structural invariants.

## Why they're gated

These tests depend on fixture files that live OUTSIDE this module. They are
kept in this repo so they travel with the code they verify, but are excluded
from `go test ./...` so the default build works from a fresh clone without
fixture setup.

## When to run

Run them on any significant package update — version bump of dependencies,
refactor of orchestrator or engine, change to guard/chain evaluation, new plan
type added. They are the regression safety net copied from the origin
(instant-scan-api) extraction.

## How to run

### Default: PARTIAL_VIEW_SETUP workspace layout

The integration tests default to paths relative to the `PARTIAL_VIEW_SETUP`
monorepo where this library is developed:

```
<workspace>/
├── golang-runspec/                    this module
└── data/
    ├── plans/all.json                 plan fixtures
    └── tests/fixtures/*.json          engine fixtures
```

From the `golang-runspec/` checkout:

```
go test -tags=integration ./...
```

If the fixture files are absent, the tests `t.Skip` with a message pointing at
the missing path rather than failing — so a standalone clone of this library
stays green.

### Override: external fixture locations

When running in a different layout (e.g. CI with fixtures checked out
separately), point the tests at the fixture files via env vars:

```
export GOLANG_RUNSPEC_PLANS=/path/to/all.json
export GOLANG_RUNSPEC_FIXTURES=/path/to/tests/fixtures
go test -tags=integration ./...
```

Both variables are read once at test-time by `engine_v2_test.go`,
`orchestrator/{integration,orchestrator}_test.go`, and
`parser/n8n/alljson_test.go`.

## Default tests

`go test ./...` (no tag) runs only the unit tests — they are self-contained
and require no external fixtures.
