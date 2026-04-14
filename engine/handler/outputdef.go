package handler

import (
	"encoding/json"
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
)

// HandleOutputDef collects output values and terminates execution.
// Reads both the primary output (from planType) and extra outputs.
func HandleOutputDef(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.OutputConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	resultMap := make(map[string]any)

	// 1. Primary output (plan type default: "oid" for OID_GEN, "value" for POST_PROCESSING)
	if primaryName := config.PrimaryOutputName(); primaryName != "" && config.PrimaryValue != "" {
		val, err := resolveExpression(config.PrimaryValue, state)
		if err != nil {
			return nil, fmt.Errorf("primary output %q: %w", primaryName, err)
		}
		resultMap[primaryName] = val
	}

	// 2. Extra outputs (legacy or additional fields)
	for _, out := range config.Outputs.Output {
		val, err := resolveExpression(out.Value, state)
		if err != nil {
			return nil, err
		}
		resultMap[out.Name] = val
	}

	state.InstructionPointer = ""

	return &maestro.ExecutionResult{
		Status:         maestro.StatusCompleted,
		PrimitiveValue: resultMap,
	}, nil
}
