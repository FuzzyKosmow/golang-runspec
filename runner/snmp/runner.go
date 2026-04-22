package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"

	"go.opentelemetry.io/otel/trace"
)

// Actions this runner supports. Services use these constants
// instead of magic strings when calling the orchestrator.
//
// Usage:
//
//	orchestrator.Run(ctx, snmp.ActionGET, scope, keys, inputs)
const (
	ActionGET  = "GET"  // SNMP GET — read a single OID value
	ActionSET  = "SET"  // SNMP SET — write a value to an OID
	ActionWALK = "WALK" // SNMP WALK — enumerate an OID subtree
)

// Plan types claimed by this runner.
const (
	// PlanTypeOIDGen is the primary SNMP plan type (graph → OID → SNMP fetch).
	PlanTypeOIDGen = "OID_GEN"

	// PlanTypePOSTProcessing is the SNMP-specific post-processing plan type.
	// Chain input from the parent OID_GEN plan is injected under "snmp_value".
	// Renamed from "POST_PROCESSING" in v0.1.4 so each runner owns its own
	// post-processing type; legacy "POST_PROCESSING" plans remain functional
	// but fall through to the orchestrator default "result" key.
	PlanTypePOSTProcessing = "POST_PROC_SNMP"
)

// planConfig is decoded from plan.Config to extract SNMP-specific fields.
type planConfig struct {
	DeviceType  int    `json:"deviceType"`
	DeviceTypes []int  `json:"deviceTypes,omitempty"`
	Property    string `json:"property"`
	PlanKey     string `json:"planKey"`
}

func decodePlanConfig(plan *maestro.Plan) planConfig {
	var cfg planConfig
	if len(plan.Config) > 0 {
		json.Unmarshal(plan.Config, &cfg)
	}
	return cfg
}

// Runner implements orchestrator.Runner for SNMP-based plan execution.
// For OID_GEN plans: execute plan → extract OID → SNMP fetch → raw value.
// For other plans (LOGIC, POST_PROC_SNMP): execute directly via engine.
//
// Shared by any service that needs SNMP: scan API, RX updater, bulk scanner.
type Runner struct {
	eng    *engine.Engine
	client Client
}

// NewRunner creates an SNMP runner with the given SNMP client.
func NewRunner(client Client) *Runner {
	eng := engine.New()
	eng.RegisterStandardHandlers()
	return &Runner{eng: eng, client: client}
}

func (r *Runner) Run(ctx context.Context, plan *maestro.Plan, action string, scope int, inputs map[string]any) (any, error) {
	span := trace.SpanFromContext(ctx)

	switch plan.Type {
	case PlanTypeOIDGen:
		state := maestro.NewState(inputs)
		result, err := r.eng.Execute(plan, state)
		if err != nil {
			return nil, fmt.Errorf("oid gen failed: %w", err)
		}
		if result.Status != maestro.StatusCompleted {
			return nil, fmt.Errorf("oid gen did not complete (status: %s)", result.Status)
		}

		oidStr, err := extractOID(result.PrimitiveValue)
		if err != nil {
			return nil, fmt.Errorf("oid extract failed: %w", err)
		}

		if r.client == nil {
			return nil, fmt.Errorf("snmp client not configured")
		}
		target, err := buildTarget(inputs, oidStr)
		if err != nil {
			return nil, err
		}

		// Action determines SNMP operation
		switch action {
		case "GET":
			vals, err := r.client.GetWithAlias(target)
			if err != nil {
				return nil, fmt.Errorf("snmp get failed: %w", err)
			}

			rawVal := extractValue(vals)
			if rawVal == nil {
				cfg := decodePlanConfig(plan)
				return nil, fmt.Errorf("snmp returned no value for %s (oid: %s)", cfg.Property, oidStr)
			}

			span.AddEvent(fmt.Sprintf("SNMP %s: %s → %v (%T)", action, oidStr, rawVal, rawVal))
			return rawVal, nil

		case "WALK":
			// Future: SNMP WALK implementation
			return nil, fmt.Errorf("WALK not yet implemented")

		case "SET":
			// Future: SNMP SET implementation
			return nil, fmt.Errorf("SET not yet implemented")

		default:
			return nil, fmt.Errorf("unsupported action %q for OID_GEN plan", action)
		}

	default:
		return r.execPlan(plan, inputs)
	}
}

// RunMany executes many (plan, inputs) pairs, grouping OID_GEN invocations by
// SNMP target (dslamIp + community) so each unique target costs ONE bulk
// GetWithAlias regardless of how many ONUs/properties feed into it. This is
// what gives the worker per-OLT mega-batch throughput without exposing
// "OLT" / "contract" concepts to the runner — grouping is a side effect of
// the standard Inputs keys (dslamIp, snmp).
//
// Per-target HTTP/SNMP failures and per-invocation OID-gen failures land as
// `error` values in the results map (partial-success); only systemic problems
// (missing client, etc.) are returned as the second value.
func (r *Runner) RunMany(ctx context.Context, action string, scope int, invocations []orchestrator.Invocation) (map[string]any, error) {
	span := trace.SpanFromContext(ctx)
	results := make(map[string]any, len(invocations))

	type oidEntry struct {
		invKey string
		oid    string
	}
	type targetKey struct {
		ip        string
		community string
	}

	groups := make(map[targetKey][]oidEntry)
	var nonOID []orchestrator.Invocation

	// Phase 1: per-invocation OID_GEN execution. Each OID_GEN plan runs
	// against its own inputs (so per-ONU slotID/portID/onuID parameters
	// produce per-ONU OIDs even when bundled into one bulk).
	for _, inv := range invocations {
		if inv.Plan == nil {
			results[inv.Key] = fmt.Errorf("nil plan for invocation %s", inv.Key)
			continue
		}
		if inv.Plan.Type != PlanTypeOIDGen {
			nonOID = append(nonOID, inv)
			continue
		}

		state := maestro.NewState(inv.Inputs)
		result, err := r.eng.Execute(inv.Plan, state)
		if err != nil {
			results[inv.Key] = fmt.Errorf("oid gen: %w", err)
			continue
		}
		if result.Status != maestro.StatusCompleted {
			results[inv.Key] = fmt.Errorf("oid gen did not complete (status: %s)", result.Status)
			continue
		}
		oidStr, err := extractOID(result.PrimitiveValue)
		if err != nil {
			results[inv.Key] = fmt.Errorf("oid extract: %w", err)
			continue
		}

		ip, community, err := extractTransport(inv.Inputs)
		if err != nil {
			results[inv.Key] = err
			continue
		}
		tk := targetKey{ip: ip, community: community}
		groups[tk] = append(groups[tk], oidEntry{invKey: inv.Key, oid: oidStr})
	}

	// Phase 2: per-target bulk SNMP fetch.
	if len(groups) > 0 && r.client == nil {
		return nil, fmt.Errorf("snmp client missing for batch fetch (%d targets queued)", len(groups))
	}
	for tk, entries := range groups {
		oidsByAlias := make(map[string]string, len(entries))
		for _, e := range entries {
			oidsByAlias[e.invKey] = e.oid
		}
		target := Target{IP: tk.ip, Community: tk.community, OIDs: oidsByAlias}

		span.AddEvent(fmt.Sprintf("SNMP RunMany %s: %d OIDs → %s", action, len(entries), tk.ip))

		snmpResults, err := r.client.GetWithAlias(target)
		if err != nil {
			// Per-target failure: every invocation pinned to this target
			// gets the same error so callers can attribute it.
			for _, e := range entries {
				results[e.invKey] = fmt.Errorf("snmp bulk fetch %s: %w", tk.ip, err)
			}
			continue
		}

		for _, e := range entries {
			rawVal := snmpResults[e.invKey]
			if rawVal == nil {
				rawVal = snmpResults[e.oid]
			}
			results[e.invKey] = rawVal
			span.AddEvent(fmt.Sprintf("  %s: oid=%s raw=%v (%T)", e.invKey, e.oid, rawVal, rawVal))
		}
	}

	// Phase 3: non-OID_GEN plans (POST_PROC etc.) execute via engine,
	// one at a time with their own inputs.
	for _, inv := range nonOID {
		val, err := r.execPlan(inv.Plan, inv.Inputs)
		if err != nil {
			results[inv.Key] = fmt.Errorf("exec: %w", err)
			continue
		}
		results[inv.Key] = val
	}

	return results, nil
}

// extractTransport pulls the SNMP target identity (IP + community) from a
// generic inputs map. Accepts both dslamIp/DslamIP and snmp/SNMP variants.
// The returned (ip, community) pair is what RunMany groups invocations by.
func extractTransport(inputs map[string]any) (string, string, error) {
	ip, _ := inputs["dslamIp"].(string)
	if ip == "" {
		ip, _ = inputs["DslamIP"].(string)
	}
	community, _ := inputs["snmp"].(string)
	if community == "" {
		community, _ = inputs["SNMP"].(string)
	}
	if ip == "" || community == "" {
		return "", "", fmt.Errorf("missing dslamIp or snmp community in inputs")
	}
	return ip, community, nil
}

func (r *Runner) SupportsBatch() bool {
	return r.client != nil
}

func (r *Runner) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           "snmp",
		AllowedActions: []string{"GET", "SET", "WALK"},
		Inputs: []orchestrator.ContractInput{
			{Key: "dslamIp", Type: "string", Required: true, NonEmpty: true, Description: "DSLAM/OLT IP address for SNMP target"},
			{Key: "snmp", Type: "string", Required: true, NonEmpty: true, Description: "SNMP community string"},
			{Key: "slotID", Type: "number", Required: false, Description: "Slot ID (0 if device has no slots, null-safe)"},
			{Key: "portID", Type: "number", Required: false, Description: "Port ID (null-safe)"},
			{Key: "onuID", Type: "number", Required: false, Description: "ONU ID (null-safe)"},
		},
		PlanIO: map[string]orchestrator.PlanTypeIO{
			PlanTypeOIDGen: {
				DefaultAction:   "GET",
				ContextInputs:   nil, // uses service inputs directly (slotID, portID, onuID)
				RequiredOutputs: []string{"oid"}, // runner reads result["oid"] for SNMP fetch
			},
			PlanTypePOSTProcessing: {
				DefaultAction: "EXECUTE",
				ContextInputs: []orchestrator.ContractInput{
					{Key: "snmp_value", Type: "any", Required: true, Description: "Raw SNMP value, injected by orchestrator from runner result"},
				},
				RequiredOutputs: []string{"value"}, // final mapped result read by frontend
			},
		},
	}
}

func (r *Runner) execPlan(plan *maestro.Plan, inputs map[string]any) (any, error) {
	state := maestro.NewState(inputs)
	for i := 0; i < 100; i++ {
		result, err := r.eng.Execute(plan, state)
		if err != nil {
			return nil, err
		}
		switch result.Status {
		case maestro.StatusCompleted:
			return result.PrimitiveValue, nil
		case maestro.StatusPaused:
			return nil, fmt.Errorf("plan paused (not supported in runner)")
		case maestro.StatusFailed:
			return nil, fmt.Errorf("plan failed")
		}
	}
	return nil, fmt.Errorf("execution limit reached")
}

// --- Helpers ---

func extractOID(value any) (string, error) {
	if m, ok := value.(map[string]any); ok {
		if oid, exists := m["oid"]; exists {
			return fmt.Sprintf("%v", oid), nil
		}
	}
	if str, ok := value.(string); ok {
		return str, nil
	}
	return "", fmt.Errorf("cannot extract OID from result type %T", value)
}

func extractValue(vals map[string]any) any {
	if v, ok := vals["value"]; ok {
		return v
	}
	if len(vals) == 1 {
		for _, v := range vals {
			return v
		}
	}
	return nil
}

func buildTarget(inputs map[string]any, oid string) (Target, error) {
	ip, _ := inputs["dslamIp"].(string)
	if ip == "" {
		ip, _ = inputs["DslamIP"].(string)
	}
	community, _ := inputs["snmp"].(string)
	if community == "" {
		community, _ = inputs["SNMP"].(string)
	}
	if ip == "" || community == "" {
		return Target{}, fmt.Errorf("missing dslamIp or snmp community in inputs")
	}
	return Target{IP: ip, Community: community, OIDs: map[string]string{"value": oid}}, nil
}

// BuildBulkTarget constructs a target for fetching multiple OIDs at once.
func BuildBulkTarget(inputs map[string]any, oidBatch map[string]string) (Target, error) {
	ip, _ := inputs["dslamIp"].(string)
	if ip == "" {
		ip, _ = inputs["DslamIP"].(string)
	}
	community, _ := inputs["snmp"].(string)
	if community == "" {
		community, _ = inputs["SNMP"].(string)
	}
	if ip == "" || community == "" {
		return Target{}, fmt.Errorf("missing dslamIp or snmp community in inputs")
	}
	oids := make(map[string]string, len(oidBatch))
	for alias, oid := range oidBatch {
		oids[alias] = oid
	}
	return Target{IP: ip, Community: community, OIDs: oids}, nil
}
