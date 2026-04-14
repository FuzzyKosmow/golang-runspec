package maestro

// InputNodeConfig for dslInputDef
type InputNodeConfig struct {
	PlanType  string `json:"planType,omitempty"`  // OID_GEN, POST_PROCESSING, CUSTOM
	ValueType string `json:"valueType,omitempty"` // number, string, bytes (POST_PROCESSING only)
	Fields    struct {
		Field []InputField `json:"field"`
	} `json:"fields"`
}

// PlanTypeDefaults is intentionally empty — defaults are defined by each runner's Contract().
// The n8n UI uses PLAN_TYPE_DEFAULTS in InputDef.node.ts (TypeScript side).
// The Go engine does NOT enforce plan type defaults — only validates fields declared in rawDefinition.
func PlanTypeDefaults(planType string, valueType ...string) []InputField {
	return nil
}

type InputField struct {
	Name         string `json:"name"`
	Type         string `json:"type"`                    // "string", "number", "boolean", "propertyRef"
	Required     bool   `json:"required"`
	PropertyName string `json:"propertyName,omitempty"` // only for type="propertyRef": resolved dependency key
}

// WorkflowLoaderConfig for dslWorkflowLoader
type WorkflowLoaderConfig struct {
	TargetWorkflowId string `json:"workflowId"`
	// Mappings for inputs to the sub-workflow
	InputMappings struct {
		Mapping []MappingItem `json:"mapping"`
	} `json:"inputMappings"`
	// Prefix for output variables
	OutputPrefix string `json:"outputPrefix"`

	// Legacy or Optional: ActionType ("READ", "WRITE")
	// If missing, implied by context or defaulted.
	// In the new TS, I don't see ActionType.
	// But the user mentioned it previously.
	// Let's keep it if it's there, but rely on usage.
	ActionType string `json:"actionType,omitempty"`
}

type MappingItem struct {
	TargetKey   string `json:"targetKey"`
	SourceValue string `json:"sourceValue"`
}

// SwitchMapConfig for dslSwitchMap
type SwitchMapConfig struct {
	Value       string `json:"value"`       // Expression/Variable
	TargetField string `json:"targetField"` // Where to store result
	Rules       struct {
		Rule []SwitchRule `json:"rule"`
	} `json:"rules"`
	DefaultValue any `json:"defaultValue"` // string or number
}

type SwitchRule struct {
	Key    string `json:"key"`
	Output any    `json:"output"` // string or number
}

// SetNodeConfig for n8n-nodes-base.set
type SetNodeConfig struct {
	Assignments struct {
		Assignments []SetAssignment `json:"assignments"`
	} `json:"assignments"`
}

type SetAssignment struct {
	Name  string `json:"name"`
	Value any    `json:"value"` // Expression string or constant
	Type  string `json:"type"`
}

// IfNodeConfig for n8n-nodes-base.if
type IfNodeConfig struct {
	Conditions struct {
		Conditions []IfCondition `json:"conditions"`
	} `json:"conditions"`
}

type IfCondition struct {
	Value1    string `json:"value1"`
	Operation string `json:"operation"`
	Value2    string `json:"value2"`
}

// OutputConfig for dslOutputDef
type OutputConfig struct {
	PlanType          string `json:"planType,omitempty"`
	PrimaryFieldName  string `json:"primaryFieldName,omitempty"`     // e.g. "oid"
	PrimaryFieldNameP string `json:"primaryFieldNamePost,omitempty"` // e.g. "value" (n8n uses separate field per plan type)
	PrimaryValue      string `json:"primaryValue,omitempty"`         // expression for the primary output

	Outputs struct {
		Output []OutputItem `json:"output"`
	} `json:"outputs"`

	EnableChaining bool   `json:"enableChaining"`
	NextWorkflowId string `json:"nextWorkflowId,omitempty"`
}

// PrimaryOutputName returns the primary output field name.
// Reads from whichever field the n8n UI set (no plan type logic here).
func (c *OutputConfig) PrimaryOutputName() string {
	if c.PrimaryFieldName != "" {
		return c.PrimaryFieldName
	}
	if c.PrimaryFieldNameP != "" {
		return c.PrimaryFieldNameP
	}
	return ""
}

type OutputItem struct {
	Name  string `json:"name"`
	Value string `json:"value"` // Expression or Static
}

// DataTransformConfig for CUSTOM.dslDataTransform
type DataTransformConfig struct {
	Operation     string `json:"operation"`     // "BYTES_TO_HEX", "HEX_TO_STRING", "STRING_MANIPULATION", "REGEX_EXTRACT"
	InputField    string `json:"inputField"`    // Field to process
	OutputField   string `json:"outputField"`   // Where to store result
	Template      string `json:"template"`      // For STRING_MANIPULATION
	Regex         string `json:"regex"`         // For REGEX_EXTRACT
	RegexTemplate string `json:"regexTemplate"` // For REGEX_EXTRACT (e.g. "$1")
}
