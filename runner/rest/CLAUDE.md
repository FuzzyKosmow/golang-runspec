# runner/rest — REST/RESTCONF Transport Runner

## What it does
Executes `REST_PROPERTY` plans: runs the plan graph to build a URL path + response field,
resolves the server from a `TRANSPORT_CONFIG` document, makes the HTTP call, extracts the value.

**Zero vendor-specific code.** All vendor specifics (URL patterns, auth, servers) live in:
- Plans (URL paths, response fields) — authored in n8n
- Transport configs (auth, servers, region mapping) — managed by admin UI, stored in same MongoDB collection

## Plan Type: REST_PROPERTY
```
Plan graph executes → outputs { "url": "/nokia-altiplano-av/.../oper-status", "responseField": "ietf-interfaces:oper-status", "transform": "" }
Plan.Config.transport = "nokia" → runner loads __TRANSPORT_NOKIA doc → resolves server + auth
Runner: full URL = server + url path → HTTP GET → parse JSON → extract field → apply transform → return
```

## Plan Type: TRANSPORT_CONFIG (not executed)
Config documents stored alongside plans in ParsedN8nWorkflows collection.
Runner loads them by name via PlanProvider. Managed by admin UI, not n8n.
```json
{
  "name": "__TRANSPORT_NOKIA",
  "type": "TRANSPORT_CONFIG",
  "config": {
    "transportKey": "nokia",
    "auth": "Basic YWRt...",
    "accept": "application/yang-data+json",
    "timeout": 10,
    "servers": { "MB": { "primary": "172.19.9.40", "secondary": "172.19.9.45" }, ... },
    "regionMap": { "FTN": "MB" },
    "defaultRegion": "MN"
  }
}
```

## Contract
- **Actions**: GET
- **Required inputs**: `hostname` (OLT name), `parent` (region, e.g. "FTN")
- **PlanIO**: `REST_PROPERTY` (outputs "url", "responseField"), `TRANSPORT_CONFIG` (skipped during execution)
- Note: `slotID`, `portID`, `onuID` are also available in inputs (passed by service) — plan graph uses them to build the URL path

## Server Resolution
```
inputs["parent"] = "FTN"
→ transport.regionMap["FTN"] = "MB"
→ transport.servers["MB"].primary = "https://172.19.9.40"
→ full URL = "https://172.19.9.40" + plan's url path
→ if primary fails, try secondary
```

## Batch Support
`RunBatch()` deduplicates by resolved full URL. TX+RX share the diagnostics endpoint → 1 HTTP call.
All REST plans in one batch share the same transport config (same device type = same vendor).

## Transforms
Inline numeric transforms (no post-processing plans needed):
- `div256` — temperature (raw / 256 = Celsius)
- `div10` — power dBm (raw / 10)
- `div100` — percentage values

## Transport Cache
Transport configs are cached in-memory by transport name.
`ClearTransportCache()` invalidates all cached configs — called when plan cache is cleared.

## Adding a new REST vendor (zero code changes)
1. Create a TRANSPORT_CONFIG document in MongoDB with the vendor's auth + servers
2. Create REST_PROPERTY plans in n8n with the vendor's URL patterns
3. Set plan.Config.transport = vendor name
4. Done — runner auto-loads the transport config, resolves servers, makes calls

## Construction
```go
restRunner := rest.NewRunner(httpClient, planProvider)
orch.RegisterRunner(restRunner)
```
- httpClient: stdlib *http.Client (shared, no vendor config)
- planProvider: for loading TRANSPORT_CONFIG docs (same interface used for plans)
