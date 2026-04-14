package n8n_test

import (
	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
	"os"
	"testing"
)

func TestParseSamplePlan(t *testing.T) {
	// Read sample file
	data, err := os.ReadFile("sample_plan.json")
	if err != nil {
		t.Fatalf("Failed to read sample file: %v", err)
	}

	plan, err := n8n.Parse(data)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Basic Validation
	if len(plan.Nodes) != 6 {
		t.Errorf("Expected 6 nodes, got %d", len(plan.Nodes))
	}

	startNode := plan.Nodes[plan.Start]
	if startNode.Name != "Start: RX Inputs" {
		t.Errorf("Expected start node to be 'Start: RX Inputs', got '%s'", startNode.Name)
	}

	// Check Connection: Start -> If Slot
	if len(startNode.Outputs["main"]) == 0 {
		t.Fatal("Start node has no outputs")
	}
	nextNodeID := startNode.Outputs["main"][0].NodeID
	nextNode := plan.Nodes[nextNodeID]
	if nextNode.Name != "If Slot == 1" {
		t.Errorf("Expected next node to be 'If Slot == 1', got '%s'", nextNode.Name)
	}

	// Check Connection: If Slot -> Map IfIndex (Slot 1) (index 0)
	if len(nextNode.Outputs["main"]) < 2 {
		t.Fatal("If Node should have 2 outputs (True/False or separate routes)")
	}
	
	// Based on n8n structure, "If" nodes usually have "main" output with index 0 (true) and index 1 (false)
	// OR they split into different connection arrays.
	// In the sample JSON:
	// "If Slot == 1": { "main": [ [ {node: Map 1} ], [ {node: Map 2} ] ] }
	// This means Route 0 is Map 1, Route 1 is Map 2.
	
	route0ID := nextNode.Outputs["main"][0].NodeID
	if plan.Nodes[route0ID].Name != "Map IfIndex (Slot 1)" {
		t.Errorf("Expected Route 0 to be 'Map IfIndex (Slot 1)', got '%s'", plan.Nodes[route0ID].Name)
	}
	
	route1ID := nextNode.Outputs["main"][1].NodeID
	if plan.Nodes[route1ID].Name != "Map IfIndex (Slot 2)" {
		t.Errorf("Expected Route 1 to be 'Map IfIndex (Slot 2)', got '%s'", plan.Nodes[route1ID].Name)
	}
}

func TestParsePostProc(t *testing.T) {
	data, err := os.ReadFile("sample_postproc.json")
	if err != nil {
		t.Fatalf("Failed to read sample file: %v", err)
	}

	plan, err := n8n.Parse(data)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(plan.Nodes) != 3 {
		t.Errorf("Expected 3 nodes, got %d", len(plan.Nodes))
	}
}
