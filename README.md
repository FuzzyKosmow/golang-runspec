# golang-plan-runner-package

A transport-agnostic plan execution engine for Go. Services declare plans as
graphs of nodes with per-action RunSpecs (guards, chains, runner dispatch); the
orchestrator evaluates guards, hands off to a pluggable runner (SNMP, REST,
database, queue, or anything else implementing the runner contract), then
executes post-step chains. Designed for services that need to orchestrate
multi-step, config-driven workflows without hard-coding the transport.

Originally extracted from the infra-metrics instant-scan API, where it drives
over 100 SNMP and REST plans across GPON device types.

## Concepts at a glance

- **Plan** — a graph plus a RunSpec. Describes what to do.
- **RunSpec** — per-action (GET, SET, WALK, EXECUTE, etc.) configuration:
  guards, chains, inputs, outputs, runner hints.
- **Guard** — a precondition evaluated before the runner is invoked.
  Built-in: MUST_EQUAL, MUST_EXIST, OVERWRITE. Supports short-circuit.
- **Chain** — a post-step action evaluated after the runner succeeds.
  Built-in: RUN_PLAN (chain to a follow-up plan).
- **Engine** — stateless graph resolver that walks nodes and hands each to a
  handler (input-def, output-def, expr, if, switch, data-transform, set).
- **Orchestrator** — top-level coordinator. Fetches a plan, evaluates guards,
  invokes the runner, executes chains, returns results.
- **Runner** — transport implementation. Each runner declares a Contract
  (supported actions, PlanIO shape) and is invoked by the orchestrator via
  a small interface. SNMP and REST runners are included.

## Package layout

```
plan.go, state.go, config.go    core types
orchestrator/                    guards -> runner dispatch -> chains
engine/                          stateless graph resolver
  handler/                       7 node handlers
parser/n8n/                      n8n JSON -> Plan
runner/snmp/                     SNMP runner (GET / SET / WALK)
runner/rest/                     REST runner (vendor-agnostic, transport config)
```

## Install

```
go get git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package@latest
```

## Quick start

```go
import (
    planrunner "git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
    "git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/orchestrator"
    "git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/runner/snmp"
)

// 1. Load a plan (from n8n JSON, database, or hand-constructed).
plan := loadPlan(...)  // returns *planrunner.Plan

// 2. Register the runners you need.
orch := orchestrator.New()
orch.RegisterRunner(snmp.NewRunner(snmpClient))
// orch.RegisterRunner(rest.NewRunner(httpClient))

// 3. Execute an action against the plan.
result, err := orch.Execute(ctx, plan, "GET", inputs)
```

## Status

Extracted from `infra-metrics-instant-scan-api` at `pkg/maestro/`. Production-
tested against GPON D5, D8/9/12, D13, D14, D17 device types. REST runner is
vendor-agnostic and used for Nokia D19 Altiplano in the origin project.

## License

MIT. See `LICENSE`.
