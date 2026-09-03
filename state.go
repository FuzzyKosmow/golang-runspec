package maestro

// ExecutionStatus defines the current state of the workflow execution.
type ExecutionStatus string

const (
	StatusCompleted ExecutionStatus = "COMPLETED"
	StatusPaused    ExecutionStatus = "PAUSED" // Missing input/dependency
	StatusFailed    ExecutionStatus = "FAILED"
)

// ExecutionState represents the serializable state of a workflow execution.
// It allows the engine to pause and resume.
type ExecutionState struct {
	// Current variables available to the workflow
	Context map[string]any `json:"context"`

	// ID of the next node to execute (if empty, starts at InputDef)
	InstructionPointer string `json:"instruction_pointer"`

	// Track execution path for debugging/loops
	VisitedNodes []string `json:"visited_nodes"`

	// Stack for handling scopes (e.g., inside decision branches)
	ScopeStack []Scope `json:"scope_stack"`

	NodeOutputs map[string]map[string]any `json:"node_outputs"`
}

// Scope represents a localized variable context (e.g. inside a loop or branch).
// Currently a placeholder for future scoping logic.
type Scope struct {
	ID        string                 `json:"id"`
	Variables map[string]interface{} `json:"variables"`
}

// ExecutionResult is returned by the Engine after every Execute() call.
type ExecutionResult struct {
	Status ExecutionStatus

	// The Snapshot to save (if async) or feed back (if sync)
	State *ExecutionState

	// The final calculated value from the plan graph.
	PrimitiveValue any

	// Intents: actions the runner/service needs to take (e.g., fetch dependency, execute command).
	Intents []Intent
}

// IntentType defines what the Engine needs from the Service.
type IntentType string

const (
	IntentFetchDependency IntentType = "FETCH_DEPENDENCY"
	IntentExecuteCommand  IntentType = "EXECUTE_COMMAND"
)

// Intent represents a command or dependency request from the Engine.
type Intent struct {
	Type IntentType

	// For FETCH_DEPENDENCY
	DependencyKey string         // what to fetch
	ResultKey     string         // where to store the result in Context
	PlanID        string         // optional: specific plan to use
	Arguments     map[string]any // inputs for the dependency

	// For EXECUTE_COMMAND
	Command *CommandDefinition
}

// CommandDefinition defines a side-effect to be executed by the runner.
// Fields are generic — each runner interprets them per its domain.
type CommandDefinition struct {
	Action     string         // runner-specific action name (e.g., "SET", "WRITE", "SEND")
	Target     string         // target identifier (runner interprets: OID, table name, endpoint, etc.)
	Value      any            // the value to write/send
	Priority   int            // execution priority (0 = default)
	Parameters map[string]any // additional runner-specific parameters
}

// NewState creates a fresh execution state with initial inputs.
//
// ⚠ THE INPUTS ARE CLONED, NOT ALIASED. Context is mutable execution state —
// SET, SWITCH_MAP and DATA_TRANSFORM all write into it — so aliasing the
// caller's map silently turned it into shared mutable state. Callers routinely
// hold one inputs map and hand it to several runs: Orchestrator.Run resolves
// dependencies concurrently on a single map, and a consumer building one
// Invocation per property from one map would have those run in parallel too.
// Either way, two goroutines touching this map is `fatal error: concurrent map
// read and map write` — unrecoverable, process gone. It killed the scan-worker
// roughly hourly until 2026-09-03.
//
// Shallow, matching orchestrator.CloneMap (duplicated rather than imported:
// orchestrator imports this package, so reusing it would be an import cycle).
// Nested maps stay shared; plan inputs are scalars today.
func NewState(inputs map[string]any) *ExecutionState {
	ctx := make(map[string]any, len(inputs)+4)
	for k, v := range inputs {
		ctx[k] = v
	}
	return &ExecutionState{
		Context:            ctx,
		InstructionPointer: "", // Will start at Start Node
		VisitedNodes:       make([]string, 0),
		ScopeStack:         make([]Scope, 0),
		NodeOutputs:        make(map[string]map[string]any),
	}
}
