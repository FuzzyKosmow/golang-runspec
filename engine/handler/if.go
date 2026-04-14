package handler

import (
	"encoding/json"
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
)

// HandleIf evaluates conditions and routes to true/false output path.
func HandleIf(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.IfNodeConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	match := true
	for _, cond := range config.Conditions.Conditions {
		val1, err := resolveExpression(cond.Value1, state)
		if err != nil {
			return nil, err
		}

		val2, err := resolveExpression(cond.Value2, state)
		if err != nil {
			return nil, err
		}

		isEqual := ToString(val1) == ToString(val2)

		if cond.Operation == "equal" && !isEqual {
			match = false
			break
		}
	}

	outputs, ok := node.Outputs["main"]
	if !ok || len(outputs) == 0 {
		state.InstructionPointer = ""
		return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
	}

	if match {
		if len(outputs) > 0 {
			state.InstructionPointer = outputs[0].NodeID
		}
	} else {
		if len(outputs) > 1 {
			state.InstructionPointer = outputs[1].NodeID
		} else {
			state.InstructionPointer = ""
		}
	}

	return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
}
