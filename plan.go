package maestro

import "encoding/json"

// Plan represents the internal graph structure optimized for execution.
// It is runner-agnostic: RunSpec is per-action, Config is opaque per runner.
type Plan struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // plan type (runner-specific, e.g. "OID_GEN", "POST_PROCESSING")
	Description string `json:"description"`
	Version     int    `json:"version"`

	// RunSpec maps action verbs to their execution specs (guards + chains).
	// Action verbs (GET, SET, WALK, EXECUTE) are defined by the runner contract.
	// Example: runspec["GET"] = { guards: [MUST_EQUAL OperStatus=Online], chains: [RUN_PLAN post_proc] }
	RunSpec map[string]ActionSpec `json:"runspec,omitempty"`

	// Config is runner-specific (opaque). Each runner decodes into its own struct.
	// SNMP runner decodes: {deviceTypes, planKey, ...}
	// Kafka runner would decode: {topic, eventType, ...}
	Config json.RawMessage `json:"config,omitempty"`

	// The execution graph
	Nodes map[string]Node `json:"nodes"`
	Start string          `json:"start"` // ID of the starting node
}

// ActionSpec defines how to run a plan for a specific action verb.
// Guards are checked before execution, chains run after.
// EXECUTE (running the graph + calling the runner) is implicit between guards and chains.
type ActionSpec struct {
	Guards []Guard `json:"guards,omitempty"`
	Chains []Chain `json:"chains,omitempty"`
}

// Guard defines a precondition checked before execution.
// Action determines behavior, Params is action-specific.
//
// Supported checks:
//   - MUST_EQUAL:  resolve Key from context, gate on value matching params.expect[]
//   - MUST_EXIST:  resolve Key from context, gate on successful resolution
//   - OVERWRITE:   resolve Key, inject into context (with optional short-circuit)
type Guard struct {
	Check  string          `json:"check"`
	Key    string          `json:"key"`              // dependency to resolve
	Params json.RawMessage `json:"params,omitempty"` // check-specific parameters
}

// Chain defines a post-execution step. Do determines behavior, Params is step-specific.
//
// Supported steps:
//   - RUN_PLAN:    execute a post-processing plan with the raw result
//   - EMIT_EVENT:  (future) publish an event to a message bus
type Chain struct {
	Do     string          `json:"do"`
	Params json.RawMessage `json:"params,omitempty"` // step-specific parameters
}

// --- Guard param types ---

// MustEqualParams are parameters for the MUST_EQUAL guard.
type MustEqualParams struct {
	Expect  []string `json:"expect"`            // allowed values
	OnFail  string   `json:"onFail,omitempty"`  // SKIP, WARN, ABORT (default: SKIP) — when value doesn't match
	OnError string   `json:"onError,omitempty"` // SKIP, WARN (default: SKIP) — when dependency can't be resolved (timeout)
	Message string   `json:"message,omitempty"` // user-facing reason
}

// MustExistParams are parameters for the MUST_EXIST guard.
type MustExistParams struct {
	OnFail  string `json:"onFail,omitempty"`  // SKIP, WARN, ABORT
	Message string `json:"message,omitempty"`
}

// OverwriteParams are parameters for the OVERWRITE guard.
type OverwriteParams struct {
	As      string   `json:"as"`                // alias to inject resolved value as
	Expect  []string `json:"expect,omitempty"`  // if set + Result set: short-circuit when matched
	Result  string   `json:"result,omitempty"`  // short-circuit value (skip runner entirely)
	OnFail  string   `json:"onFail,omitempty"`  // WARN to continue on resolution failure
	OnError string   `json:"onError,omitempty"` // SKIP, WARN (default: SKIP) — when dependency can't be resolved (timeout)
}

// --- Chain param types ---

// RunPlanParams are parameters for the RUN_PLAN chain step.
type RunPlanParams struct {
	PlanKey string `json:"planKey"`
}

// Node represents a single step in the workflow.
type Node struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // "dslInputDef", "dslSwitchMap", etc.

	Parameters json.RawMessage `json:"parameters"`

	Outputs map[string][]Connection `json:"outputs"`
}

// Connection represents a link to another node.
type Connection struct {
	NodeID     string `json:"nodeId"`
	InputIndex int    `json:"inputIndex"`
}

// GetActionSpec returns the ActionSpec for a given action verb.
// Returns nil if the plan has no runspec for that action.
func (p *Plan) GetActionSpec(action string) *ActionSpec {
	if p.RunSpec == nil {
		return nil
	}
	spec, ok := p.RunSpec[action]
	if !ok {
		return nil
	}
	return &spec
}

// HasChains returns true if the plan has any chains for the given action.
func (p *Plan) HasChains(action string) bool {
	spec := p.GetActionSpec(action)
	return spec != nil && len(spec.Chains) > 0
}
