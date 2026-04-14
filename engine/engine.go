package engine

import (
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/engine/handler"
)

// NodeHandler is the function signature for node type handlers.
type NodeHandler = handler.HandlerFunc

// Engine is the stateless execution runtime.
type Engine struct {
	handlers map[string]NodeHandler
	debug    bool
}

// New creates a new Engine with an empty handler registry.
func New() *Engine {
	return &Engine{
		handlers: make(map[string]NodeHandler),
	}
}

// RegisterStandardHandlers registers all built-in handlers (InputDef, If, Set, etc.).
func (e *Engine) RegisterStandardHandlers() {
	for name, h := range handler.All() {
		e.handlers[name] = h
	}
}

// RegisterHandler registers a single handler for a node type.
func (e *Engine) RegisterHandler(nodeType string, h NodeHandler) {
	e.handlers[nodeType] = h
}

// SetDebug enables or disables console logging of execution steps.
func (e *Engine) SetDebug(enabled bool) {
	e.debug = enabled
}

// Execute runs the plan from the current state until it completes, pauses, or fails.
func (e *Engine) Execute(plan *maestro.Plan, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	stepCount := 0
	for {
		stepCount++
		currentID := state.InstructionPointer
		if currentID == "" {
			currentID = plan.Start
			state.InstructionPointer = currentID
		}

		if e.debug {
			fmt.Printf("[ENGINE] Step %d: Node ID=%s\n", stepCount, currentID)
		}

		node, ok := plan.Nodes[currentID]
		if !ok {
			return &maestro.ExecutionResult{Status: maestro.StatusFailed}, fmt.Errorf("node not found: %s", currentID)
		}

		if e.debug {
			fmt.Printf("[ENGINE] Node Name: %s, Type: %s\n", node.Name, node.Type)
		}

		h, ok := e.handlers[node.Type]
		if !ok {
			return &maestro.ExecutionResult{Status: maestro.StatusFailed}, fmt.Errorf("handler not found for type: %s", node.Type)
		}

		result, err := h(node, state)
		if err != nil {
			if e.debug {
				fmt.Printf("[ENGINE] Error: %v\n", err)
			}
			return &maestro.ExecutionResult{Status: maestro.StatusFailed}, fmt.Errorf("error executing node %s: %w", node.Name, err)
		}

		if e.debug {
			fmt.Printf("[ENGINE] Result Status: %s\n", result.Status)
		}

		if result.Status == maestro.StatusPaused || result.Status == maestro.StatusFailed {
			return result, nil
		}

		if result.Status == maestro.StatusCompleted {
			snapshot := make(map[string]any)
			for k, v := range state.Context {
				snapshot[k] = v
			}
			if state.NodeOutputs == nil {
				state.NodeOutputs = make(map[string]map[string]any)
			}
			state.NodeOutputs[node.Name] = snapshot

			if state.InstructionPointer == "" || state.InstructionPointer == "END" {
				return result, nil
			}
		}
	}
}
