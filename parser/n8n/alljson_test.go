//go:build integration
// +build integration

package n8n_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	n8n "git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/parser/n8n"
)

// allJSONPath resolves the path relative to the repo root.
var allJSONPath = func() string {
	// Walk up from this test file to the repo root (infra-metrics-instant-scan-api/)
	// then resolve the sibling directory.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	return filepath.Join(repoRoot, "..", "SampleExecutionPlan-StoredInMongo", "plans", "all.json")
}()

// planDocument mirrors the top-level MongoDB document shape (only the fields we validate).
type planDocument struct {
	ID            json.RawMessage            `json:"_id"`
	Name          string                     `json:"name"`
	Type          string                     `json:"type"`
	IsActive      bool                       `json:"isActive"`
	SchemaVersion int                        `json:"schemaVersion"`
	UIType        string                     `json:"uiType"`
	RunSpec       map[string]json.RawMessage `json:"runspec"`
	Config        struct {
		DeviceTypes []int  `json:"deviceTypes"`
		Property    string `json:"property"`
		PlanKey     string `json:"planKey"`
	} `json:"config"`
	RawDefinition json.RawMessage `json:"rawDefinition"`
}

// validCleanTypes are the node type names the engine knows how to handle.
// After normalization, every node type MUST appear in this set.
var validCleanTypes = map[string]bool{
	"InputDef":       true,
	"If":             true,
	"SwitchMap":      true,
	"Set":            true,
	"OutputDef":      true,
	"WorkflowLoader": true,
	"DataTransform":  true,
}

// TestAllJSONPlans reads every plan in all.json and validates structure + parseability.
func TestAllJSONPlans(t *testing.T) {
	data, err := os.ReadFile(allJSONPath)
	if err != nil {
		t.Fatalf("Failed to read all.json: %v", err)
	}

	var docs []planDocument
	if err := json.Unmarshal(data, &docs); err != nil {
		t.Fatalf("Failed to unmarshal all.json as array: %v", err)
	}

	if len(docs) == 0 {
		t.Fatal("all.json is empty — expected at least one plan document")
	}

	t.Logf("Loaded %d plan documents from all.json", len(docs))

	var failures []string

	for i, doc := range docs {
		label := fmt.Sprintf("[%d] %s", i, doc.Name)
		var errs []string

		// --- 0. TRANSPORT_CONFIG is metadata, not an executable plan — skip plan validation ---
		if doc.Type == "TRANSPORT_CONFIG" {
			if doc.Config.PlanKey == "" {
				errs = append(errs, "config.planKey is empty (needed for lookup)")
			}
			if len(errs) > 0 {
				msg := fmt.Sprintf("%s (TRANSPORT_CONFIG): %s", label, strings.Join(errs, "; "))
				failures = append(failures, msg)
				t.Errorf("%s", msg)
			}
			continue
		}

		// --- 1. schemaVersion ---
		if doc.SchemaVersion != 3 {
			errs = append(errs, fmt.Sprintf("schemaVersion = %d, want 3", doc.SchemaVersion))
		}

		// --- 2. runspec must exist and have at least GET ---
		if doc.RunSpec == nil {
			errs = append(errs, "runspec is missing")
		} else if _, ok := doc.RunSpec["GET"]; !ok {
			errs = append(errs, "runspec.GET is missing")
		} else {
			// Validate GET action spec structure
			var spec struct {
				Guards []json.RawMessage `json:"guards"`
				Chains []json.RawMessage `json:"chains"`
			}
			if err := json.Unmarshal(doc.RunSpec["GET"], &spec); err != nil {
				errs = append(errs, fmt.Sprintf("runspec.GET is not valid: %v", err))
			}
		}

		// --- 3. config.planKey ---
		if doc.Config.PlanKey == "" {
			errs = append(errs, "config.planKey is empty or missing")
		}

		// --- 4. rawDefinition must be parseable ---
		if doc.RawDefinition == nil {
			errs = append(errs, "rawDefinition is missing")
		} else {
			plan, parseErr := n8n.Parse(doc.RawDefinition)
			if parseErr != nil {
				errs = append(errs, fmt.Sprintf("n8n.Parse failed: %v", parseErr))
			} else {
				// --- 5. parsed plan must have a Start node ---
				if plan.Start == "" {
					errs = append(errs, "parsed plan has empty Start")
				} else if _, ok := plan.Nodes[plan.Start]; !ok {
					errs = append(errs, fmt.Sprintf("Start ID %q not found in Nodes map", plan.Start))
				}

				// --- 6. all node types must be clean names (no CUSTOM.dsl* / n8n-nodes-base.*) ---
				for nodeID, node := range plan.Nodes {
					if strings.HasPrefix(node.Type, "CUSTOM.") || strings.HasPrefix(node.Type, "n8n-nodes-base.") {
						errs = append(errs, fmt.Sprintf("node %s (%s) has un-normalized type %q", nodeID, node.Name, node.Type))
					}
					if !validCleanTypes[node.Type] {
						errs = append(errs, fmt.Sprintf("node %s (%s) has unknown type %q (not in handler registry)", nodeID, node.Name, node.Type))
					}
				}
			}
		}

		// --- 7. Additional structural checks ---
		validDocTypes := map[string]bool{
			"OID_GEN":          true,
			"POST_PROCESSING":  true,
			"POST_PROC":        true,
			"REST_PROPERTY":    true,
			"TRANSPORT_CONFIG": true,
		}
		if !validDocTypes[doc.Type] {
			errs = append(errs, fmt.Sprintf("unexpected document type %q", doc.Type))
		}

		if len(errs) > 0 {
			msg := fmt.Sprintf("%s (planKey=%s):\n  - %s", label, doc.Config.PlanKey, strings.Join(errs, "\n  - "))
			failures = append(failures, msg)
			t.Errorf("%s", msg)
		}
	}

	// Summary
	t.Logf("\n=== ALL.JSON VALIDATION SUMMARY ===")
	t.Logf("Total plans: %d", len(docs))
	t.Logf("Failed:      %d", len(failures))
	t.Logf("Passed:      %d", len(docs)-len(failures))

	if len(failures) > 0 {
		t.Logf("\nFailed plans:")
		for _, f := range failures {
			t.Logf("  %s", f)
		}
	}
}

// TestD19RESTPlansExecution loads all D19 REST_PROPERTY plans, parses them,
// and verifies they have correct structure (InputDef→Set→OutputDef) with
// proper REST runner configuration and responseField.
func TestD19RESTPlansExecution(t *testing.T) {
	data, err := os.ReadFile(allJSONPath)
	if err != nil {
		t.Skipf("all.json not found at %s: %v", allJSONPath, err)
	}

	var docs []planDocument
	if err := json.Unmarshal(data, &docs); err != nil {
		t.Fatalf("Failed to parse all.json: %v", err)
	}

	var restDocs []planDocument
	for _, d := range docs {
		if d.Type == "REST_PROPERTY" {
			for _, dt := range d.Config.DeviceTypes {
				if dt == 19 {
					restDocs = append(restDocs, d)
					break
				}
			}
		}
	}

	if len(restDocs) == 0 {
		t.Skip("No D19 REST_PROPERTY plans found")
	}
	t.Logf("Found %d D19 REST_PROPERTY plans", len(restDocs))

	for _, doc := range restDocs {
		t.Run(doc.Config.PlanKey, func(t *testing.T) {
			plan, err := n8n.Parse(doc.RawDefinition)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
			if plan.Start == "" {
				t.Fatal("No start node")
			}
			if len(plan.Nodes) != 3 {
				t.Errorf("Expected 3 nodes (InputDef→Set→OutputDef), got %d", len(plan.Nodes))
			}

			// Verify start node
			startNode := plan.Nodes[plan.Start]
			if startNode.Type != "InputDef" {
				t.Errorf("Start type = %q, want InputDef", startNode.Type)
			}
			var startP struct {
				Runner   string `json:"runner"`
				PlanType string `json:"planType"`
			}
			json.Unmarshal(startNode.Parameters, &startP)
			if startP.Runner != "rest" {
				t.Errorf("Start runner = %q, want rest", startP.Runner)
			}

			// Find OutputDef and verify responseField
			for _, node := range plan.Nodes {
				if node.Type != "OutputDef" {
					continue
				}
				var outP struct {
					ResponseField string `json:"responseField"`
					Transform     string `json:"transform"`
					PrimaryValue  string `json:"primaryValue"`
				}
				json.Unmarshal(node.Parameters, &outP)

				if outP.ResponseField == "" {
					t.Error("OutputDef missing responseField")
				}
				if outP.PrimaryValue == "" {
					t.Error("OutputDef missing primaryValue (URL expression)")
				}

				t.Logf("  %s → field=%s transform=%q url_expr=%.80s",
					doc.Config.Property, outP.ResponseField, outP.Transform, outP.PrimaryValue)
			}

			// Find Set node and verify it builds urlPath with hostname
			for _, node := range plan.Nodes {
				if node.Type != "Set" {
					continue
				}
				raw := string(node.Parameters)
				if !strings.Contains(raw, "urlPath") {
					t.Error("Set node should build urlPath")
				}
				if !strings.Contains(raw, "hostname") {
					t.Error("Set node URL should reference hostname")
				}
			}
		})
	}
}
