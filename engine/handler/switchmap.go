package handler

import (
	"encoding/json"
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
)

// HandleSwitchMap maps an input value to a target field via a rule table.
func HandleSwitchMap(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.SwitchMapConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	valRaw, err := resolveExpression(config.Value, state)
	if err != nil {
		return nil, err
	}
	valStr := ToString(valRaw)

	matched := false
	for _, rule := range config.Rules.Rule {
		if rule.Key == valStr {
			resolved, err := resolveExpression(ToString(rule.Output), state)
			if err != nil {
				return nil, fmt.Errorf("switchMap rule output resolve failed: %w", err)
			}
			state.Context[config.TargetField] = TryParseNumber(resolved)
			matched = true
			break
		}
	}

	if !matched {
		resolved, err := resolveExpression(ToString(config.DefaultValue), state)
		if err != nil {
			return nil, fmt.Errorf("switchMap defaultValue resolve failed: %w", err)
		}
		state.Context[config.TargetField] = TryParseNumber(resolved)
	}

	advancePointer(node, state)
	return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
}
