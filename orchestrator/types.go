package orchestrator

// Result represents the outcome of resolving a single item.
type Result struct {
	Name         string        `json:"name"`
	Status       Status        `json:"status"`
	Value        any           `json:"value,omitempty"`
	Error        string        `json:"error,omitempty"`
	Logs         []string      `json:"logs,omitempty"`
	GuardResults []GuardResult `json:"guardResults,omitempty"`
}

// GuardResult represents the outcome of a single guard evaluation.
// Distinct from service-level prechecks (ping, OSP, account partner) which are
// request-level gates. Guard results are plan-level dependency gates.
type GuardResult struct {
	Key      string `json:"key"`
	Check    string `json:"check"` // MUST_EQUAL, MUST_EXIST, OVERWRITE
	Status   Status `json:"status"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Message  string `json:"message"`
}

// Status represents the execution status.
type Status string

const (
	StatusSuccess     Status = "SUCCESS"
	StatusError       Status = "ERROR"
	StatusSkipped     Status = "SKIPPED"
	StatusUnsupported Status = "UNSUPPORTED"
)
