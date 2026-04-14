# pkg/maestro — General-Purpose Plan Orchestration Engine

## Intent
This is NOT an SNMP library or a scanner-specific package. It is a **general-purpose plan execution engine** meant to be imported by any Go service that needs plan-driven orchestration.

The package is transport-agnostic and domain-agnostic. It handles:
- Synchronous I/O (SNMP GET, REST calls, DB queries)
- Asynchronous workflows (Kafka events, ticket requests, approval flows)
- Any operation expressible as: fetch plan → check guards → execute → post-process

### Example consuming services
| Service | Runner | Plans do what |
|---------|--------|--------------|
| OLT scanner (current) | SNMP, REST | Read device properties via SNMP/RESTCONF |
| Kafka consumer | Kafka | Process events, transform, route |
| DB updater | SQL | Execute update plans against databases |
| Ticket system | HTTP | Create/update tickets via API |
| Bulk processor | Batch | Scan thousands of devices in parallel |

Each service imports `pkg/maestro/`, registers its runner(s), provides plans and inputs. The orchestrator handles everything else: guard evaluation, dependency resolution, execution dispatch, chain post-processing, tracing.

## Core Abstractions

**Plan** = a graph of nodes + a RunSpec (per-action guards + chains) + opaque config.
Plans are domain-specific data, stored externally (MongoDB, filesystem, etc.).

**Runner** = the "do" part. Executes a plan's core I/O. SNMP fetches OIDs. REST calls URLs. Kafka produces/consumes. The orchestrator never knows what transport a runner uses.

**Guard** = precondition. "Only run this if OperStatus is Online." "Inject this dependency value." Generic — works for any domain.

**Chain** = post-step. "After getting the raw value, run this transform plan." Generic — RUN_PLAN today, EMIT_EVENT or UPDATE_DB in the future.

**CapabilityGate** = "is this key allowed for this scope?" An allowlist. Could be SQL, config, ACL table. Optional.

**Action** = a verb the service chooses ("GET", "SET", "WALK", "EXECUTE", "PROCESS"). Service-level intent. The runner interprets it per its transport.

## Package Layout
```
maestro/
  plan.go, state.go, config.go    — Core types (Plan, Node, ActionSpec, Guard, Chain)
  orchestrator/                    — Guards → engine → runner dispatch → chains
    orchestrator.go                — Multi-runner dispatch, full execution pipeline
    interfaces.go                  — PlanProvider, Runner, CapabilityGate, RunnerContract
    types.go                       — Result, GuardResult, Status
    helpers.go                     — CoerceNumeric, FormatValue, CloneMap
  engine/                          — Stateless graph resolver (executes plan node graphs)
    handler/                       — 7 node type handlers (switchmap, set, math, etc.)
  parser/n8n/                      — n8n JSON → Plan converter (one possible plan source)
  runner/
    snmp/                          — SNMP transport runner (ships with package)
    rest/                          — REST/RESTCONF transport runner (ships with package)
```

## Execution Flow
```
Service calls: orchestrator.Run(action, scope, keys, inputs)
  Phase 0: CapabilityGate — filter allowed keys
  Phase 1: PlanProvider.GetPlans(scope, keys) — fetch plans by scope+key
  Phase 2: Discover guard dependencies from plan RunSpec
  Phase 3: Resolve dependencies concurrently (via correct runner per plan)
  Phase 4: Evaluate guards (MUST_EQUAL, MUST_EXIST, OVERWRITE + short-circuit)
  Phase 5: Execute — grouped by runner (batch or single per runner)
  Phase 6: Apply chains (POST_PROCESSING via engine, future: EMIT_EVENT, etc.)
```

**Action is service-level.** The service says "GET" — the orchestrator routes each plan to the correct runner by `plan.Type`. The runner interprets the action per its transport (SNMP GET, HTTP GET, Kafka produce, DB SELECT — all "GET" from the service's perspective).

**Scope and keys are domain-specific.** For SNMP: scope=deviceType, key=propertyName. For Kafka: scope=topic, key=eventType. The orchestrator doesn't interpret them.

## Runner Contract
```go
type RunnerContract struct {
    Name           string              // "snmp", "rest", "kafka", "sql"
    AllowedActions []string            // what this runner can do
    Inputs         []ContractInput     // what the service must provide
    PlanIO         map[string]PlanTypeIO // plan types this runner handles
}
```
The contract serves multiple purposes:
- **Dispatch**: PlanIO keys tell the orchestrator which plan types this runner handles
- **Validation**: AllowedActions + Inputs checked at call time (warnings, not blocking)
- **Documentation**: Service developers know what inputs to provide
- **UI generation**: n8n node editor can use PlanIO to enforce correct field names

## Multi-Runner Dispatch
```go
orch := orchestrator.NewOrchestrator(plans, defaultRunner, gate)
orch.RegisterRunner(restRunner)  // reads Contract().PlanIO, auto-registers by plan type
```
- `RegisterRunner(runner)` reads `runner.Contract().PlanIO` keys → registers all
- `runnerForPlan(plan)` dispatches by `plan.Type`, falls back to default
- Phase 5 groups plans by runner so each runner can batch its own type
- No manual plan-type-to-runner mapping needed

## Plan Types (currently shipped)
| Type | Runner | Description |
|------|--------|-------------|
| `OID_GEN` | SNMP | Plan graph computes OID → SNMP fetch → raw value |
| `POST_PROCESSING` | Engine only | Chain transform: raw value → human-readable |
| `REST_PROPERTY` | REST | Plan graph builds URL → HTTP GET → extract field |

New plan types are added by creating a new runner that declares them in its Contract().PlanIO.

## Key Design Principles
1. **Truly agnostic** — not an SNMP library. Not a scanner. A plan execution engine.
2. **Plan-type-driven dispatch** — `plan.Type` determines which runner, not service code
3. **No domain branching** — no `isNokia()`, no `isKafka()`. Plans carry all specifics.
4. **Service provides inputs, picks action** — runners pick what they need, ignore the rest
5. **Package ships runners** — services configure, not construct. Pending: config-driven orchestrator.
6. **Guards and chains are generic** — work for any domain, any runner, any transport
7. **Extensible by convention** — new runner = new package under `runner/`, declares contract, done

## What does NOT belong in this package
- Domain-specific business logic (precheck ping, bypass detection, contract validation)
- Service-level routing (HTTP handlers, middleware)
- Database schemas or migrations
- Config file parsing (Vault, env vars)

These live in the consuming service's `internal/` layer.
