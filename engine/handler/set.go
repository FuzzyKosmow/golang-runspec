package handler

import (
	"encoding/json"
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
)

// HandleSet assigns values to context variables.
func HandleSet(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.SetNodeConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	for _, assignment := range config.Assignments.Assignments {
		val, err := resolveExpression(fmt.Sprintf("%v", assignment.Value), state)
		if err != nil {
			return nil, err
		}

		if assignment.Type == "number" {
			val = TryParseNumber(val)
		} else if assignment.Type == "string" {
			val = ToString(val)
		}

		state.Context[assignment.Name] = val
	}

	advancePointer(node, state)
	return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
}
