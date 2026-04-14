package engine_test

import (
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/engine"
	"git.fpt.net/isc/thong-so-l2-l3/infra-metrics/packages/golang-plan-runner-package/parser/n8n"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionSamplePlan(t *testing.T) {
	// 1. Load Plan
	path := filepath.Join("..", "parser", "n8n", "sample_plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read plan: %v", err)
	}
	plan, err := n8n.Parse(data)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// 2. Setup Engine
	eng := engine.New()
	eng.RegisterStandardHandlers()

	// 3. Prepare Inputs (Case 1: Slot 1 -> Should map to .285278470 for port 6)
	// Rules: Slot 1, Port 6 -> Key "6" -> ".285278470"
	// Formula: .1.3.6.1.4.1.3902.1082.500.20.2.2.2.1.10{ifindex}.{onuID}.1
	// .1.3.6.1.4.1.3902.1082.500.20.2.2.2.1.10.285278470.7.1
	
	inputs := map[string]any{
		"slotID": 1,
		"portID": 6,
		"onuID":  7,
	}
	state := maestro.NewState(inputs)

	// 4. Execute
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}

	// 5. Validate
	if result.Status != maestro.StatusCompleted {
		t.Errorf("Expected status COMPLETED, got %s", result.Status)
	}

	resMap, ok := result.PrimitiveValue.(map[string]any)
	if !ok {
		t.Fatalf("Expected map result, got %T", result.PrimitiveValue)
	}

	oid, ok := resMap["oid"]
	if !ok {
		t.Fatal("Result missing 'oid' field")
	}

	expectedOID := ".1.3.6.1.4.1.3902.1082.500.20.2.2.2.1.10.285278470.7.1"
	if oid != expectedOID {
		t.Errorf("Expected OID '%s', got '%s'", expectedOID, oid)
	}
}

func TestExecutionPostProc(t *testing.T) {
	path := filepath.Join("..", "parser", "n8n", "sample_postproc.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read plan: %v", err)
	}
	plan, err := n8n.Parse(data)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	eng := engine.New()
	eng.RegisterStandardHandlers()

	// Logic: snmp_value * 0.002 - 30
	// Input: 2000 -> 2000*0.002 = 4 -> 4-30 = -26
	inputs := map[string]any{
		"snmp_value": 2000,
	}
	state := maestro.NewState(inputs)

	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Execution failed: %v", err)
	}

	resMap, ok := result.PrimitiveValue.(map[string]any)
	if !ok {
		t.Fatalf("Expected map result")
	}
	
	val := resMap["value"]
	// expr returns float64 usually
	if valFloat, ok := val.(float64); ok {
		if valFloat != -26.0 {
			t.Errorf("Expected -26.0, got %f", valFloat)
		}
	} else {
		// Or string?
		t.Logf("Got value type: %T, value: %v", val, val)
	}
}

