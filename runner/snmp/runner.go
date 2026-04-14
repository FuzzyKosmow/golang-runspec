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
// For other plans (LOGIC, POST_PROCESSING): execute directly via engine.
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
	case "OID_GEN":
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

func (r *Runner) RunBatch(ctx context.Context, plans map[string]*maestro.Plan, action string, scope int, inputs map[string]any) (map[string]any, error) {
	span := trace.SpanFromContext(ctx)
	results := make(map[string]any)
	oidBatch := make(map[string]string)
	var nonOIDKeys []string

	for key, plan := range plans {
		if plan.Type == "OID_GEN" {
			state := maestro.NewState(inputs)
			result, err := r.eng.Execute(plan, state)
			if err != nil {
				return nil, fmt.Errorf("oid gen %s: %w", key, err)
			}
			if result.Status != maestro.StatusCompleted {
				return nil, fmt.Errorf("oid gen %s did not complete", key)
			}
			oidStr, err := extractOID(result.PrimitiveValue)
			if err != nil {
				return nil, fmt.Errorf("oid extract %s: %w", key, err)
			}
			oidBatch[key] = oidStr
		} else {
			nonOIDKeys = append(nonOIDKeys, key)
		}
	}

	if len(oidBatch) > 0 {
		if r.client == nil {
			return nil, fmt.Errorf("snmp client missing for batch fetch")
		}
		target, err := BuildBulkTarget(inputs, oidBatch)
		if err != nil {
			return nil, fmt.Errorf("bulk target build: %w", err)
		}

		span.AddEvent(fmt.Sprintf("SNMP Batch %s: %d OIDs → %s", action, len(oidBatch), target.IP))

		snmpResults, err := r.client.GetWithAlias(target)
		if err != nil {
			return nil, fmt.Errorf("bulk snmp fetch: %w", err)
		}

		for key, oid := range oidBatch {
			rawVal := snmpResults[key]
			if rawVal == nil {
				rawVal = snmpResults[oid]
			}
			results[key] = rawVal
			span.AddEvent(fmt.Sprintf("  %s: oid=%s raw=%v (%T)", key, oid, rawVal, rawVal))
		}
	}

	for _, key := range nonOIDKeys {
		val, err := r.execPlan(plans[key], inputs)
		if err != nil {
			return nil, fmt.Errorf("exec %s: %w", key, err)
		}
		results[key] = val
	}

	return results, nil
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
			"OID_GEN": {
				DefaultAction:   "GET",
				ContextInputs:   nil, // uses service inputs directly (slotID, portID, onuID)
				RequiredOutputs: []string{"oid"}, // runner reads result["oid"] for SNMP fetch
			},
			"POST_PROCESSING": {
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
