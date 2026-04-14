//go:build integration
// +build integration

package engine_test

import (
	"encoding/json"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/parser/n8n"
	"os"
	"path/filepath"
	"testing"
)

// loadProdFixture loads a production plan fixture by planKey suffix.
func loadProdFixture(t *testing.T, filename string) (*maestro.Plan, *V3Document) {
	t.Helper()
	path := filepath.Join(testPlanDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read %s: %v", filename, err)
	}

	var doc V3Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Failed to parse %s: %v", filename, err)
	}

	plan, err := n8n.Parse(doc.RawDefinition)
	if err != nil {
		t.Fatalf("Failed to parse rawDefinition from %s: %v", filename, err)
	}
	return plan, &doc
}

// ============================================================
// Production Plan: D5 RX — OID Generation
// Input: portID=2, onuID=3
// Expected OID: .1.3.6.1.4.1.13464.1.11.4.1.1.22.0.2.3
// ============================================================

func TestProd_D5_RX_OIDGen(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_oid_gen_d5_rx.json")
	eng := newEngine()

	inputs := map[string]any{
		"portID": float64(2),
		"onuID":  float64(3),
	}
	state := maestro.NewState(inputs)
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}
	if result.Status != maestro.StatusCompleted {
		t.Fatalf("Expected COMPLETED, got %s", result.Status)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	oid := resMap["oid"]
	expected := ".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.2.3"
	if oid != expected {
		t.Errorf("D5 RX OID:\n  got:  %s\n  want: %s", oid, expected)
	}
}

func TestProd_D5_RX_OIDGen_DifferentPort(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_oid_gen_d5_rx.json")
	eng := newEngine()

	inputs := map[string]any{
		"portID": float64(8),
		"onuID":  float64(15),
	}
	state := maestro.NewState(inputs)
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	expected := ".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.8.15"
	if resMap["oid"] != expected {
		t.Errorf("D5 RX OID port 8 onu 15:\n  got:  %s\n  want: %s", resMap["oid"], expected)
	}
}

// ============================================================
// Production Plan: D8/9/12 OperStatus — OID Generation + POST_PROC chain
// Simulates the full pipeline: generate OID → (mock SNMP) → post-process
// ============================================================

func TestProd_D8_OperStatus_OIDGen(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_oid_gen_d8_9_12_operstatus.json")
	eng := newEngine()

	inputs := map[string]any{
		"slotID": float64(1),
		"portID": float64(2),
		"onuID":  float64(3),
	}
	state := maestro.NewState(inputs)
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	expected := ".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.6.1.2.3"
	if resMap["oid"] != expected {
		t.Errorf("D8 OperStatus OID:\n  got:  %s\n  want: %s", resMap["oid"], expected)
	}
}

func TestProd_D8_OperStatus_PostProc_Online(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_d8_9_12_operstatus_post_proc.json")
	eng := newEngine()

	// SNMP returned 1 → should map to "Online"
	inputs := map[string]any{"snmp_value": float64(1)}
	state := maestro.NewState(inputs)
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	if resMap["value"] != "Online" {
		t.Errorf("Expected 'Online' for snmp_value=1, got '%v'", resMap["value"])
	}
}

func TestProd_D8_OperStatus_PostProc_Offline(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_d8_9_12_operstatus_post_proc.json")
	eng := newEngine()

	inputs := map[string]any{"snmp_value": float64(0)}
	state := maestro.NewState(inputs)
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	if resMap["value"] != "Offline" {
		t.Errorf("Expected 'Offline' for snmp_value=0, got '%v'", resMap["value"])
	}
}

func TestProd_D8_OperStatus_PostProc_AllValues(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_d8_9_12_operstatus_post_proc.json")
	eng := newEngine()

	cases := []struct {
		input    float64
		expected string
	}{
		{0, "Offline"},
		{1, "Online"},
		{2, "Lawless"},
		{3, "Unregister"},
		{99, "Unknown"}, // default
	}

	for _, tc := range cases {
		t.Run(tc.expected, func(t *testing.T) {
			inputs := map[string]any{"snmp_value": tc.input}
			state := maestro.NewState(inputs)
			result, err := eng.Execute(plan, state)
			if err != nil {
				t.Fatalf("Execution failed for input %v: %v", tc.input, err)
			}
			resMap := result.PrimitiveValue.(map[string]any)
			if resMap["value"] != tc.expected {
				t.Errorf("snmp_value=%v: got '%v', want '%s'", tc.input, resMap["value"], tc.expected)
			}
		})
	}
}

// ============================================================
// Production Plan: D5 OperStatus — Full chain
// ============================================================

func TestProd_D5_OperStatus_PostProc_AllValues(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_d5_operstatus_post_proc.json")
	eng := newEngine()

	cases := []struct {
		input    float64
		expected string
	}{
		{0, "Offline"},
		{1, "Online"},
		{2, "Lawless"},
		{3, "Unregister"},
		{99, "Unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.expected, func(t *testing.T) {
			inputs := map[string]any{"snmp_value": tc.input}
			state := maestro.NewState(inputs)
			result, err := eng.Execute(plan, state)
			if err != nil {
				t.Fatalf("Execution failed: %v", err)
			}
			resMap := result.PrimitiveValue.(map[string]any)
			if resMap["value"] != tc.expected {
				t.Errorf("snmp_value=%v: got '%v', want '%s'", tc.input, resMap["value"], tc.expected)
			}
		})
	}
}

// ============================================================
// Production Plan: D5 OnStatus — Complex 24-value SwitchMap
// ============================================================

func TestProd_D5_OnStatus_PostProc_KeyValues(t *testing.T) {
	plan, _ := loadProdFixture(t, "prod_d5_onstatus_post_proc.json")
	eng := newEngine()

	// Actual D5 OnStatus SwitchMap values (24-value map from production plan)
	cases := []struct {
		input    float64
		expected string
	}{
		{0, "power off"},
		{1, "normal"},
		{2, "los"},
		{4, "lofi"},
		{24, "Unknown"}, // not in map → default
		{672, "All of the ONT has been deactivated"},
	}

	for _, tc := range cases {
		t.Run(tc.expected, func(t *testing.T) {
			inputs := map[string]any{"snmp_value": tc.input}
			state := maestro.NewState(inputs)
			result, err := eng.Execute(plan, state)
			if err != nil {
				t.Fatalf("Execution failed for snmp_value=%v: %v", tc.input, err)
			}
			resMap := result.PrimitiveValue.(map[string]any)
			if resMap["value"] != tc.expected {
				t.Errorf("snmp_value=%v: got '%v', want '%s'", tc.input, resMap["value"], tc.expected)
			}
		})
	}
}

// ============================================================
// Simulated Full Pipeline: OID_GEN → (mock SNMP) → POST_PROC
// This simulates what the orchestrator does, without mocks
// ============================================================

func TestProd_FullPipeline_D8_OperStatus(t *testing.T) {
	// Step 1: Generate OID
	oidPlan, _ := loadProdFixture(t, "prod_oid_gen_d8_9_12_operstatus.json")
	eng := newEngine()

	oidInputs := map[string]any{
		"slotID": float64(2),
		"portID": float64(4),
		"onuID":  float64(10),
	}
	oidState := maestro.NewState(oidInputs)
	oidResult, err := eng.Execute(oidPlan, oidState)
	if err != nil {
		t.Fatalf("OID generation failed: %v", err)
	}
	oidMap := oidResult.PrimitiveValue.(map[string]any)
	generatedOID := oidMap["oid"].(string)

	expectedOID := ".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.6.2.4.10"
	if generatedOID != expectedOID {
		t.Fatalf("OID mismatch:\n  got:  %s\n  want: %s", generatedOID, expectedOID)
	}

	// Step 2: Simulate SNMP fetch (mock: device returns 1 = Online)
	snmpRawValue := float64(1)

	// Step 3: Post-process the SNMP value
	postPlan, _ := loadProdFixture(t, "prod_d8_9_12_operstatus_post_proc.json")
	postInputs := map[string]any{"snmp_value": snmpRawValue}
	postState := maestro.NewState(postInputs)
	postResult, err := eng.Execute(postPlan, postState)
	if err != nil {
		t.Fatalf("Post-processing failed: %v", err)
	}

	postMap := postResult.PrimitiveValue.(map[string]any)
	finalValue := postMap["value"]

	if finalValue != "Online" {
		t.Errorf("Full pipeline D8 OperStatus (slot=2,port=4,onu=10):\n  OID: %s\n  SNMP raw: %v\n  Final: %v (want 'Online')",
			generatedOID, snmpRawValue, finalValue)
	}

	t.Logf("Full pipeline SUCCESS: OID=%s → SNMP=%v → result=%v", generatedOID, snmpRawValue, finalValue)
}

func TestProd_FullPipeline_D5_OperStatus(t *testing.T) {
	// Step 1: Generate OID
	oidPlan, _ := loadProdFixture(t, "prod_oid_gen_d5_operstatus.json")
	eng := newEngine()

	oidInputs := map[string]any{
		"portID": float64(3),
		"onuID":  float64(7),
	}
	oidState := maestro.NewState(oidInputs)
	oidResult, err := eng.Execute(oidPlan, oidState)
	if err != nil {
		t.Fatalf("OID generation failed: %v", err)
	}
	oidMap := oidResult.PrimitiveValue.(map[string]any)
	generatedOID := oidMap["oid"].(string)

	expectedOID := ".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7"
	if generatedOID != expectedOID {
		t.Fatalf("OID mismatch:\n  got:  %s\n  want: %s", generatedOID, expectedOID)
	}

	// Step 2-3: SNMP returns 1 → POST_PROC → "Online"
	postPlan, _ := loadProdFixture(t, "prod_d5_operstatus_post_proc.json")
	postInputs := map[string]any{"snmp_value": float64(1)}
	postState := maestro.NewState(postInputs)
	postResult, err := eng.Execute(postPlan, postState)
	if err != nil {
		t.Fatalf("Post-processing failed: %v", err)
	}

	postMap := postResult.PrimitiveValue.(map[string]any)
	if postMap["value"] != "Online" {
		t.Errorf("Expected 'Online', got '%v'", postMap["value"])
	}

	t.Logf("Full pipeline D5 OperStatus: OID=%s → raw=1 → %v", generatedOID, postMap["value"])
}
