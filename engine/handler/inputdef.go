package handler

import (
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
)

// HandleInputDef validates required inputs declared in fields and advances.
// PlanType defaults are for UI documentation only — not enforced here.
// The rawDefinition's fields.field[] is the authority for what's required.
func HandleInputDef(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.InputNodeConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Only validate explicitly declared fields (from rawDefinition)
	for _, field := range config.Fields.Field {
		if field.Required {
			if _, ok := state.Context[field.Name]; !ok {
				return &maestro.ExecutionResult{Status: maestro.StatusFailed},
					fmt.Errorf("missing required input: %s", field.Name)
			}
		}
	}

	advancePointer(node, state)
	return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
}
