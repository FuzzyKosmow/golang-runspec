package n8n

import (
	"encoding/json"
	"fmt"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
	"strings"
)

// N8NWorkflow represents the raw structure exported from n8n.
type N8NWorkflow struct {
	Nodes       []N8NNode                `json:"nodes"`
	Connections map[string]N8NConnection `json:"connections"`
	Meta        map[string]any           `json:"meta"` // Name, etc.
}

type N8NNode struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
	Position   []float64       `json:"position"`
}

// N8NConnection maps "NodeName" -> "OutputName" -> List of connections
// Example: "MyNode": { "main": [ [ { "node": "NextNode", "type": "main", "index": 0 } ] ] }
// Note: n8n connections structure is complex: map[NodeName]map[OutputType][][]ConnectionItem
type N8NConnection map[string][][]ConnectionItem

type ConnectionItem struct {
	Node   string `json:"node"`
	Type   string `json:"type"`
	Index  int    `json:"index"`
}

// NormalizeNodeType strips UI-specific prefixes from n8n node types,
// producing clean engine-native names:
//
//	"CUSTOM.dslInputDef"     → "InputDef"
//	"n8n-nodes-base.if"      → "If"
//	"n8n-nodes-base.set"     → "Set"
//	"CUSTOM.dslDataTransform" → "DataTransform"
//
// Unknown prefixes pass through unchanged, so custom parsers (Camunda, etc.)
// can use their own normalization.
func NormalizeNodeType(raw string) string {
	prefixes := []string{"CUSTOM.dsl", "n8n-nodes-base."}
	for _, prefix := range prefixes {
		if strings.HasPrefix(raw, prefix) {
			name := raw[len(prefix):]
			if len(name) > 0 {
				return strings.ToUpper(name[:1]) + name[1:]
			}
			return name
		}
	}
	return raw
}

// Parse converts a raw n8n JSON byte slice into an optimized Execution Plan.
func Parse(rawJSON []byte) (*maestro.Plan, error) {
	var workflow N8NWorkflow
	if err := json.Unmarshal(rawJSON, &workflow); err != nil {
		return nil, fmt.Errorf("failed to unmarshal n8n workflow: %w", err)
	}

	plan := &maestro.Plan{
		Nodes: make(map[string]maestro.Node),
	}

	// 1. Map Nodes
	// We need a lookup for Name -> ID because n8n connections use Names, but we want IDs.
	nameToID := make(map[string]string)

	for _, n := range workflow.Nodes {
		// If ID is missing (older n8n), use Name or Generate one
		id := n.ID
		if id == "" {
			id = n.Name
		}
		nameToID[n.Name] = id

		// Convert to Core Node — normalize type to engine-native name
		nodeType := NormalizeNodeType(n.Type)
		coreNode := maestro.Node{
			ID:         id,
			Name:       n.Name,
			Type:       nodeType,
			Parameters: n.Parameters,
			Outputs:    make(map[string][]maestro.Connection),
		}
		plan.Nodes[id] = coreNode

		// Identify Start Node
		if nodeType == "InputDef" {
			plan.Start = id
		}
	}

	// 2. Map Edges
	for sourceNodeName, outputs := range workflow.Connections {
		sourceID, ok := nameToID[sourceNodeName]
		if !ok {
			continue // Should warn?
		}

		sourceNode := plan.Nodes[sourceID]

		for outputName, routes := range outputs {
			// n8n supports multiple "routes" for an output (the outer array).
			// Example: "main": [ [Route0_Connection1], [Route1_Connection1] ]
			// For "If" nodes: Route 0 = True, Route 1 = False (usually).
			
			for _, route := range routes {
				for _, connItem := range route {
					targetID, ok := nameToID[connItem.Node]
					if !ok {
						continue
					}
					
					// Store the connection
					sourceNode.Outputs[outputName] = append(sourceNode.Outputs[outputName], maestro.Connection{
						NodeID:     targetID,
						InputIndex: connItem.Index,
					})
				}
			}
		}
		// Update map
		plan.Nodes[sourceID] = sourceNode
	}

	if plan.Start == "" {
		// Fallback: If no dslInputDef, look for Trigger or start with first node?
		// For now, strict.
		return nil, fmt.Errorf("no start node (InputDef) found")
	}

	return plan, nil
}
