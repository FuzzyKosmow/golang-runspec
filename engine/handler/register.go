package handler

import "git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"

// HandlerFunc is the function signature for node handlers.
type HandlerFunc func(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error)

// All returns the complete set of standard handlers keyed by clean node type name.
// These names match the normalized types produced by n8n.NormalizeNodeType().
func All() map[string]HandlerFunc {
	return map[string]HandlerFunc{
		"InputDef":       HandleInputDef,
		"If":             HandleIf,
		"SwitchMap":      HandleSwitchMap,
		"Set":            HandleSet,
		"OutputDef":      HandleOutputDef,
		"WorkflowLoader": HandleWorkflowLoader,
		"DataTransform":  HandleDataTransform,
	}
}
