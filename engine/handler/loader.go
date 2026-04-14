package handler

import (
	"encoding/json"
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
)

// HandleWorkflowLoader pauses execution to request a sub-workflow dependency.
func HandleWorkflowLoader(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.WorkflowLoaderConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	outputKey := config.OutputPrefix
	if outputKey == "" {
		outputKey = "subResult"
	}

	// RESUME CHECK:
	if _, exists := state.Context[outputKey]; exists {
		advancePointer(node, state)
		return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
	}

	// PAUSE & REQUEST:
	resolvedInputs := make(map[string]any)
	for _, mapping := range config.InputMappings.Mapping {
		val, err := resolveExpression(mapping.SourceValue, state)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve input %s: %w", mapping.TargetKey, err)
		}
		resolvedInputs[mapping.TargetKey] = val
	}

	intent := maestro.Intent{
		Type:          maestro.IntentFetchDependency,
		DependencyKey: config.TargetWorkflowId,
		ResultKey:     outputKey,
		PlanID:        config.TargetWorkflowId,
		Arguments:     resolvedInputs,
	}

	return &maestro.ExecutionResult{
		Status:  maestro.StatusPaused,
		Intents: []maestro.Intent{intent},
	}, nil
}
