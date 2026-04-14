# runner/snmp — SNMP Transport Runner

## What it does
Executes `OID_GEN` plans: runs the plan graph to compute an OID, then performs SNMP GET/SET/WALK to fetch the raw value from the OLT/DSLAM.

## Plan Type: OID_GEN
```
Plan graph executes → outputs { "oid": "1.3.6.1.4.1.2011.6.128.1.1.2.46.1.15.XXX" }
Runner extracts OID → SNMP GET to target IP → returns raw value (int, string, []byte)
```
POST_PROCESSING plans chain after this to map raw values (e.g., 1 → "Online").

## Contract
- **Actions**: GET, SET, WALK
- **Required inputs**: `dslamIp` (OLT IP), `snmp` (community string)
- **Optional inputs**: `slotID`, `portID`, `onuID`
- **PlanIO**: `OID_GEN` (outputs "oid"), `POST_PROCESSING` (receives "snmp_value")

## Batch Support
`RunBatch()` groups all OID_GEN plans → resolves OIDs → single bulk SNMP GET with aliased OIDs → splits results back per key. Non-OID plans fall through to engine execution.

## Client Interface
```go
type Client interface {
    GetWithAlias(target Target) (map[string]any, error)
}
```
Implementation (`snmp.Executor`) handles timeouts, retries, community strings.

## Files
- `runner.go` — Runner implementation, Run/RunBatch/Contract
- `target.go` — buildTarget/BuildBulkTarget helpers
- `actions.go` — Action constants (GET, SET, WALK)
- `client.go` — Client interface + Executor implementation
