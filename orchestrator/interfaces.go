package orchestrator

import (
	"context"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
)

// PlanProvider fetches execution plans. Implementations may read from MongoDB, filesystem, cache, etc.
// Method names are intentionally generic — "scope" and "key" mean different things per domain:
//   - SNMP:    scope = deviceType, key = property name ("RX", "TX")
//   - Kafka:   scope = topic partition, key = event type
//   - Process: scope = department, key = process ID
type PlanProvider interface {
	GetPlan(ctx context.Context, scope int, key string) (*maestro.Plan, error)
	GetPlans(ctx context.Context, scope int, keys []string) (map[string]*maestro.Plan, error)
	GetPlanByKey(ctx context.Context, scope int, planKey string) (*maestro.Plan, error)
}

// Runner executes a plan's core work. This is the domain-specific part:
//   - SNMP service:  OID generation → SNMP fetch → raw value
//   - Kafka service: produce message / consume + transform
//   - Process mgr:   execute process step, await approval
//   - Bulk scanner:  batch OID gen → bulk SNMP round-trip
//
// The Orchestrator handles guards, chains, and dependency resolution.
// The Runner handles the "middle" — the actual I/O for a given plan.
type Runner interface {
	Run(ctx context.Context, plan *maestro.Plan, action string, scope int, inputs map[string]any) (any, error)
	RunBatch(ctx context.Context, plans map[string]*maestro.Plan, action string, scope int, inputs map[string]any) (map[string]any, error)
	SupportsBatch() bool
	// Contract declares this runner's capabilities: allowed actions, plan type I/O.
	// Returns nil if the runner has no specific requirements.
	Contract() *RunnerContract
}

// RunnerContract declares the capabilities and input requirements for a runner.
// Used for validation, documentation, and UI generation.
type RunnerContract struct {
	// Name identifies this runner type (e.g. "snmp", "db", "mail").
	Name string
	// AllowedActions lists the action verbs this runner supports (e.g. ["GET", "SET", "WALK"]).
	// The orchestrator rejects requests for actions not in this list.
	AllowedActions []string
	// Inputs the runner expects from the service. Some may be optional.
	Inputs []ContractInput
	// PlanIO declares what each plan type expects and produces.
	// Used by n8n UI to enforce correct InputDef/OutputDef field names.
	PlanIO map[string]PlanTypeIO
}

// PlanTypeIO declares the context inputs and required outputs for a plan type.
type PlanTypeIO struct {
	// DefaultAction is the action verb used if the service doesn't specify one.
	// e.g., "GET" for OID_GEN, "EXECUTE" for POST_PROCESSING.
	DefaultAction string
	// ContextInputs are injected by the orchestrator into the plan's execution context.
	// These are NOT from the service — the orchestrator provides them automatically.
	// e.g., POST_PROCESSING always receives "snmp_value" from the orchestrator.
	ContextInputs []ContractInput
	// RequiredOutputs are the field names the runner/orchestrator reads from the plan result.
	// e.g., OID_GEN must output "oid", POST_PROCESSING must output "value".
	RequiredOutputs []string
}

// ContractInput describes a single input field the runner expects.
type ContractInput struct {
	Key         string // e.g. "targetIP", "apiKey", "connectionString"
	Type        string // "string", "number", "any"
	Required    bool   // true = key must exist in inputs (value can be nil/zero)
	NonEmpty    bool   // true = value must also be non-zero/non-empty (e.g. target IP)
	Description string // for service writers: what this is and where to get it
}

// Validate checks inputs against the contract and returns warnings.
// Only checks key presence (Required) and non-empty values (NonEmpty).
// Returns nil if everything looks good.
func (c *RunnerContract) Validate(inputs map[string]any) []string {
	if c == nil {
		return nil
	}
	var warnings []string
	for _, input := range c.Inputs {
		val, exists := inputs[input.Key]
		if input.Required && !exists {
			warnings = append(warnings, fmt.Sprintf("missing required input %q (%s)", input.Key, input.Description))
			continue
		}
		if input.NonEmpty && exists {
			empty := false
			switch v := val.(type) {
			case string:
				empty = v == ""
			case nil:
				empty = true
			}
			if empty {
				warnings = append(warnings, fmt.Sprintf("input %q is empty (%s)", input.Key, input.Description))
			}
		}
	}
	return warnings
}

// IsActionAllowed checks if the given action verb is in the contract's AllowedActions.
func (c *RunnerContract) IsActionAllowed(action string) bool {
	if c == nil {
		return true // no contract = all actions allowed
	}
	for _, a := range c.AllowedActions {
		if a == action {
			return true
		}
	}
	return false
}

// CapabilityGate validates whether requested items are allowed before execution.
//   - SNMP/Bulk scanner: "Is property X linked to device type Y?" (SQL allowlist)
//   - Kafka processor:   "Is event X in my event catalog?"        (event registry)
//   - Process manager:   "Can department X trigger process Y?"     (ACL table)
//
// Optional — if nil, all items are considered allowed.
type CapabilityGate interface {
	GetAllowed(ctx context.Context, scope int) (map[string]bool, error)
}
