//go:build integration
// +build integration

package orchestrator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"os"
	"path/filepath"
	"testing"
)

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ============================================================
// Mock implementations of pipeline interfaces
// ============================================================

type mockPlanProvider struct {
	byScope   map[string]*maestro.Plan
	byPlanKey map[string]*maestro.Plan
}

func newMockProvider() *mockPlanProvider {
	return &mockPlanProvider{
		byScope:   make(map[string]*maestro.Plan),
		byPlanKey: make(map[string]*maestro.Plan),
	}
}

func (m *mockPlanProvider) add(deviceType int, key, planKey string, plan *maestro.Plan) {
	m.byScope[fmt.Sprintf("%d:%s", deviceType, key)] = plan
	if planKey != "" {
		m.byPlanKey[fmt.Sprintf("%d:%s", deviceType, planKey)] = plan
	}
}

func (m *mockPlanProvider) GetPlan(_ context.Context, scope int, key string) (*maestro.Plan, error) {
	if p, ok := m.byScope[fmt.Sprintf("%d:%s", scope, key)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("plan not found: %d:%s", scope, key)
}

func (m *mockPlanProvider) GetPlans(_ context.Context, scope int, keys []string) (map[string]*maestro.Plan, error) {
	result := make(map[string]*maestro.Plan)
	for _, key := range keys {
		if p, ok := m.byScope[fmt.Sprintf("%d:%s", scope, key)]; ok {
			result[key] = p
		}
	}
	return result, nil
}

func (m *mockPlanProvider) GetPlanByKey(_ context.Context, scope int, planKey string) (*maestro.Plan, error) {
	if p, ok := m.byPlanKey[fmt.Sprintf("%d:%s", scope, planKey)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("plan not found by key: %d:%s", scope, planKey)
}

// --- Engine-based executor (runs plans through the real engine) ---

type engineExecutor struct {
	eng        *engine.Engine
	snmpValues map[string]any // OID → value (simulates SNMP responses)
}

func newEngineExecutor(snmpValues map[string]any) *engineExecutor {
	eng := engine.New()
	eng.RegisterStandardHandlers()
	return &engineExecutor{eng: eng, snmpValues: snmpValues}
}

func (e *engineExecutor) Run(_ context.Context, plan *maestro.Plan, _ string, _ int, inputs map[string]any) (any, error) {
	state := maestro.NewState(inputs)
	result, err := e.eng.Execute(plan, state)
	if err != nil {
		return nil, err
	}
	if result.Status != maestro.StatusCompleted {
		return nil, fmt.Errorf("plan did not complete: %s", result.Status)
	}

	// If OID_GEN plan, simulate SNMP fetch
	if plan.Type == "OID_GEN" {
		resMap, ok := result.PrimitiveValue.(map[string]any)
		if ok {
			if oid, exists := resMap["oid"]; exists {
				oidStr := fmt.Sprintf("%v", oid)
				if val, found := e.snmpValues[oidStr]; found {
					return val, nil
				}
				return nil, fmt.Errorf("no SNMP response for OID: %s", oidStr)
			}
		}
	}

	return result.PrimitiveValue, nil
}

func (e *engineExecutor) RunBatch(_ context.Context, plans map[string]*maestro.Plan, _ string, _ int, inputs map[string]any) (map[string]any, error) {
	// Simple: execute each plan individually
	results := make(map[string]any)
	for prop, plan := range plans {
		val, err := e.Run(context.Background(), plan, "GET", 0, inputs)
		if err != nil {
			return nil, err
		}
		results[prop] = val
	}
	return results, nil
}

func (e *engineExecutor) SupportsBatch() bool { return true }
func (e *engineExecutor) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           "snmp",
		AllowedActions: []string{"GET", "SET", "WALK"},
		PlanIO: map[string]orchestrator.PlanTypeIO{
			"OID_GEN":        {DefaultAction: "GET"},
			"POST_PROC_SNMP": {DefaultAction: "EXECUTE", ContextInputs: []orchestrator.ContractInput{{Key: "snmp_value"}}},
		},
	}
}

// --- Mock meta checker ---

type mockGate struct {
	allowed map[int]map[string]bool
}

func (m *mockGate) GetAllowed(_ context.Context, dt int) (map[string]bool, error) {
	return m.allowed[dt], nil
}

// ============================================================
// Test helpers
// ============================================================

// fixtureDir points at the PARTIAL_VIEW_SETUP workspace's fixtures. Override
// with GOLANG_RUNSPEC_FIXTURES for other layouts; tests skip if absent.
var fixtureDir = func() string {
	if env := os.Getenv("GOLANG_RUNSPEC_FIXTURES"); env != "" {
		return env
	}
	return "../../data/tests/fixtures"
}()

func loadPlan(t *testing.T, filename string) *maestro.Plan {
	t.Helper()
	path := filepath.Join(fixtureDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("fixture not available at %s — set GOLANG_RUNSPEC_FIXTURES to override", path)
		}
		t.Fatalf("read %s: %v", filename, err)
	}

	var doc struct {
		Type    string          `json:"type"`
		RunSpec json.RawMessage `json:"runspec"`
		Config  json.RawMessage `json:"config"`
		RawDef  json.RawMessage `json:"rawDefinition"`
	}
	json.Unmarshal(data, &doc)

	plan, err := n8n.Parse(doc.RawDef)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	plan.Type = doc.Type
	plan.Config = doc.Config

	// Decode runspec
	if len(doc.RunSpec) > 0 {
		var rs map[string]struct {
			Guards []struct {
				Check  string          `json:"check"`
				Key    string          `json:"key"`
				Params json.RawMessage `json:"params"`
			} `json:"guards"`
			Chains []struct {
				Do     string          `json:"do"`
				Params json.RawMessage `json:"params"`
			} `json:"chains"`
		}
		json.Unmarshal(doc.RunSpec, &rs)

		plan.RunSpec = make(map[string]maestro.ActionSpec)
		for action, spec := range rs {
			as := maestro.ActionSpec{}
			for _, g := range spec.Guards {
				as.Guards = append(as.Guards, maestro.Guard{
					Check: g.Check, Key: g.Key, Params: g.Params,
				})
			}
			for _, c := range spec.Chains {
				as.Chains = append(as.Chains, maestro.Chain{
					Do: c.Do, Params: c.Params,
				})
			}
			plan.RunSpec[action] = as
		}
	}

	return plan
}

// ============================================================
// Pipeline Resolver Tests — v2 Native
// ============================================================

func TestResolver_SimpleProperty_NoPreActions(t *testing.T) {
	plan := loadPlan(t, "v2_plan_no_actions.json")

	provider := newMockProvider()
	provider.add(5, "Equipment", "TEST_V2_EQUIPMENT", plan)

	executor := newEngineExecutor(nil)
	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5, []string{"Equipment"}, map[string]any{"portID": float64(5)})
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["Equipment"]
	if r.Status != orchestrator.StatusSuccess {
		t.Errorf("Expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}
	t.Logf("Equipment result: %v", r.Value)
}

func TestResolver_D5_OperStatus_FullPipeline(t *testing.T) {
	oidPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	postPlan := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	provider := newMockProvider()
	provider.add(5, "OperStatus", "OID_GEN_D5_OPERSTATUS", oidPlan)
	provider.add(5, "", "D5_OPERSTATUS_POST_PROC", postPlan)

	// SNMP returns "1" for OperStatus OID
	executor := newEngineExecutor(map[string]any{
		".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3": "1",
	})

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"OperStatus"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["OperStatus"]
	if r.Status != orchestrator.StatusSuccess {
		t.Fatalf("Expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}

	// Post-proc maps 1 → "Online"
	valMap, ok := r.Value.(map[string]any)
	if ok {
		if valMap["value"] != "Online" {
			t.Errorf("Expected 'Online', got %v", valMap["value"])
		}
	}
	t.Logf("OperStatus: %v", r.Value)
}

func TestResolver_D5_RX_PreflightOnline(t *testing.T) {
	rxPlan := loadPlan(t, "prod_oid_gen_d5_rx.json")
	operPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	operPost := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	provider := newMockProvider()
	provider.add(5, "RX", "OID_GEN_D5_RX", rxPlan)
	provider.add(5, "OperStatus", "OID_GEN_D5_OPERSTATUS", operPlan)
	provider.add(5, "", "D5_OPERSTATUS_POST_PROC", operPost)

	executor := newEngineExecutor(map[string]any{
		".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3":  "1",     // OperStatus → Online
		".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.2.3": "-22.5", // RX value
	})

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"RX"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["RX"]
	if r.Status != orchestrator.StatusSuccess {
		t.Errorf("RX: expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}
	t.Logf("RX value: %v (preflight passed, SNMP fetched)", r.Value)
}

func TestResolver_D5_RX_PreflightOffline_Skipped(t *testing.T) {
	rxPlan := loadPlan(t, "prod_oid_gen_d5_rx.json")
	operPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	operPost := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	provider := newMockProvider()
	provider.add(5, "RX", "OID_GEN_D5_RX", rxPlan)
	provider.add(5, "OperStatus", "OID_GEN_D5_OPERSTATUS", operPlan)
	provider.add(5, "", "D5_OPERSTATUS_POST_PROC", operPost)

	// SNMP returns "0" → Offline → RX should be SKIPPED
	executor := newEngineExecutor(map[string]any{
		".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3": "0",
	})

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"RX"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["RX"]
	if r.Status != orchestrator.StatusSkipped {
		t.Errorf("RX: expected SKIPPED (device offline), got %s", r.Status)
	}
	t.Logf("RX correctly skipped: %s", r.Error)
}

func TestResolver_MultiProperty_SharedDependency(t *testing.T) {
	rxPlan := loadPlan(t, "prod_oid_gen_d5_rx.json")
	operPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	operPost := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	// TX reuses RX plan structure (same OID pattern, different property name)
	txPlan := loadPlan(t, "prod_oid_gen_d5_rx.json")
	txPlan.Config, _ = json.Marshal(map[string]any{
		"deviceType": 5, "property": "TX", "planKey": "OID_GEN_D5_TX",
	})

	provider := newMockProvider()
	provider.add(5, "RX", "OID_GEN_D5_RX", rxPlan)
	provider.add(5, "TX", "OID_GEN_D5_TX", txPlan)
	provider.add(5, "OperStatus", "OID_GEN_D5_OPERSTATUS", operPlan)
	provider.add(5, "", "D5_OPERSTATUS_POST_PROC", operPost)

	executor := newEngineExecutor(map[string]any{
		".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3":  "1",     // OperStatus
		".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.2.3": "-22.5", // RX
	})

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"RX", "TX"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	// Both should succeed — OperStatus resolved once, cached for both
	for _, prop := range []string{"RX", "TX"} {
		r := results[prop]
		if r.Status != orchestrator.StatusSuccess {
			t.Errorf("%s: expected SUCCESS, got %s (error: %s)", prop, r.Status, r.Error)
		}
		t.Logf("%s: %v", prop, r.Value)
	}
}

func TestResolver_MetaCheck_UnsupportedProperty(t *testing.T) {
	plan := loadPlan(t, "v2_plan_no_actions.json")

	provider := newMockProvider()
	provider.add(5, "Equipment", "TEST", plan)

	executor := newEngineExecutor(nil)
	meta := &mockGate{allowed: map[int]map[string]bool{
		5: {"Equipment": true}, // Temperature NOT allowed
	}}

	resolver := orchestrator.NewOrchestrator(provider, executor, meta)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"Equipment", "Temperature"},
		map[string]any{"portID": float64(1)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	if results["Equipment"].Status != orchestrator.StatusSuccess {
		t.Errorf("Equipment should succeed, got %s", results["Equipment"].Status)
	}
	if results["Temperature"].Status != orchestrator.StatusUnsupported {
		t.Errorf("Temperature should be UNSUPPORTED, got %s", results["Temperature"].Status)
	}
	t.Logf("Temperature correctly unsupported: %s", results["Temperature"].Error)
}

// ============================================================
// New v2-specific tests
// ============================================================

func TestResolver_OVERWRITE_Injection(t *testing.T) {
	// OnStatus plan with OVERWRITE pre-action: resolves OperStatus and injects as "operStatus"
	onStatusPlan := loadPlan(t, "v2_plan_overwrite.json")
	operPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	operPost := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	provider := newMockProvider()
	provider.add(8, "OnStatus", "TEST_V2_ONSTATUS", onStatusPlan)
	provider.add(8, "OperStatus", "OID_GEN_D5_OPERSTATUS", operPlan)
	provider.add(8, "", "D5_OPERSTATUS_POST_PROC", operPost)
	// The OVERWRITE plan also has a POST_PROC post-action
	var postParams maestro.RunPlanParams
	getSpec := onStatusPlan.GetActionSpec("GET")
	if getSpec != nil && len(getSpec.Chains) > 0 && json.Unmarshal(getSpec.Chains[0].Params, &postParams) == nil && postParams.PlanKey != "" {
		postProcPlan := loadPlan(t, "v2_plan_overwrite.json") // self-contained for this test
		provider.add(8, "", postParams.PlanKey, postProcPlan)
	}

	executor := newEngineExecutor(map[string]any{
		".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3": "1", // OperStatus → Online
	})

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",8,
		[]string{"OnStatus"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["OnStatus"]
	// Check that pre-action completed with OVERWRITE
	hasOverwrite := false
	for _, pc := range r.GuardResults {
		if pc.Check == "OVERWRITE" {
			hasOverwrite = true
			if pc.Status != orchestrator.StatusSuccess {
				t.Errorf("OVERWRITE pre-check should succeed, got %s: %s", pc.Status, pc.Message)
			}
			t.Logf("OVERWRITE: %s", pc.Message)
		}
	}
	if !hasOverwrite {
		t.Error("Expected OVERWRITE pre-check result, found none")
	}
	t.Logf("OnStatus result: status=%s value=%v", r.Status, r.Value)
}

func TestResolver_MUST_EXIST_Success(t *testing.T) {
	// Create a plan with MUST_EXIST pre-action
	plan := loadPlan(t, "v2_plan_no_actions.json")
	// Manually add a MUST_EXIST guard
	plan.RunSpec = map[string]maestro.ActionSpec{
		"GET": {Guards: []maestro.Guard{
			{Check: "MUST_EXIST", Key: "OperStatus", Params: mustJSON(maestro.MustExistParams{})},
		}},
	}

	operPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	operPost := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	provider := newMockProvider()
	provider.add(5, "Equipment", "TEST", plan)
	provider.add(5, "OperStatus", "OID_GEN_D5_OPERSTATUS", operPlan)
	provider.add(5, "", "D5_OPERSTATUS_POST_PROC", operPost)

	executor := newEngineExecutor(map[string]any{
		".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3": "1",
	})

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"Equipment"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["Equipment"]
	if r.Status != orchestrator.StatusSuccess {
		t.Errorf("Expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}

	// Verify MUST_EXIST pre-check passed
	for _, pc := range r.GuardResults {
		if pc.Check == "MUST_EXIST" {
			if pc.Status != orchestrator.StatusSuccess {
				t.Errorf("MUST_EXIST should succeed, got %s", pc.Status)
			}
			t.Logf("MUST_EXIST: %s", pc.Message)
		}
	}
}

func TestResolver_MUST_EXIST_Failure(t *testing.T) {
	// MUST_EXIST with a property that has no plan → should fail
	plan := loadPlan(t, "v2_plan_no_actions.json")
	plan.RunSpec = map[string]maestro.ActionSpec{
		"GET": {Guards: []maestro.Guard{
			{Check: "MUST_EXIST", Key: "NonExistentProp", Params: mustJSON(maestro.MustExistParams{})},
		}},
	}

	provider := newMockProvider()
	provider.add(5, "Equipment", "TEST", plan)
	// NOT adding NonExistentProp to provider

	executor := newEngineExecutor(nil)
	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"Equipment"},
		map[string]any{"portID": float64(1)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["Equipment"]
	if r.Status != orchestrator.StatusSkipped {
		t.Errorf("Expected SKIPPED (MUST_EXIST failed), got %s", r.Status)
	}
	t.Logf("Equipment correctly skipped: %s", r.Error)
}

func TestResolver_ByteArray_SNMP(t *testing.T) {
	// Test that []byte SNMP responses are handled correctly
	plan := loadPlan(t, "v2_plan_no_actions.json")

	provider := newMockProvider()
	provider.add(5, "Equipment", "TEST", plan)

	// Create executor that returns []byte values (simulating SNMP octet string)
	executor := &byteArrayExecutor{
		value: []byte("ZTE-C320"),
	}

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"Equipment"},
		map[string]any{"portID": float64(1)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["Equipment"]
	if r.Status != orchestrator.StatusSuccess {
		t.Errorf("Expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}
	t.Logf("Equipment (from []byte): %v (type: %T)", r.Value, r.Value)
}

func TestResolver_ByteArray_NonPrintable_SNMP(t *testing.T) {
	// Test non-printable byte arrays (e.g., MAC addresses)
	plan := loadPlan(t, "v2_plan_no_actions.json")

	provider := newMockProvider()
	provider.add(5, "MacOnu", "TEST", plan)

	// MAC address as raw bytes
	executor := &byteArrayExecutor{
		value: []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
	}

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"MacOnu"},
		map[string]any{"portID": float64(1)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["MacOnu"]
	if r.Status != orchestrator.StatusSuccess {
		t.Errorf("Expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}
	t.Logf("MacOnu (non-printable bytes): %v (type: %T)", r.Value, r.Value)
}

func TestResolver_ByteArray_Numeric_SNMP(t *testing.T) {
	// Test byte array that contains a numeric string (common SNMP pattern)
	operPlan := loadPlan(t, "prod_oid_gen_d5_operstatus.json")
	operPost := loadPlan(t, "prod_d5_operstatus_post_proc.json")

	provider := newMockProvider()
	provider.add(5, "OperStatus", "OID_GEN_D5_OPERSTATUS", operPlan)
	provider.add(5, "", "D5_OPERSTATUS_POST_PROC", operPost)

	// SNMP returns "1" as bytes — should be coerced to numeric for post-processing
	executor := &byteArrayExecutor{
		value:      []byte("1"),
		eng:        engine.New(),
		snmpValues: map[string]any{".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.2.3": []byte("1")},
	}
	executor.eng.RegisterStandardHandlers()

	resolver := orchestrator.NewOrchestrator(provider, executor, nil)

	results, err := resolver.Run(context.Background(), "GET",5,
		[]string{"OperStatus"},
		map[string]any{"portID": float64(2), "onuID": float64(3)},
	)
	if err != nil {
		t.Fatalf("ResolveProperties: %v", err)
	}

	r := results["OperStatus"]
	if r.Status != orchestrator.StatusSuccess {
		t.Fatalf("Expected SUCCESS, got %s (error: %s)", r.Status, r.Error)
	}
	t.Logf("OperStatus (from []byte '1'): %v", r.Value)
}

func TestResolver_ExtractDependencies_V2(t *testing.T) {
	// Verify ExtractDependencies reads from RunSpec[action].Guards
	plan := &maestro.Plan{
		RunSpec: map[string]maestro.ActionSpec{
			"GET": {Guards: []maestro.Guard{
				{Check: "MUST_EQUAL", Key: "OperStatus", Params: mustJSON(maestro.MustEqualParams{})},
				{Check: "OVERWRITE", Key: "Temperature", Params: mustJSON(maestro.OverwriteParams{})},
				{Check: "MUST_EXIST", Key: "Equipment", Params: mustJSON(maestro.MustExistParams{})},
			}},
		},
	}

	deps := orchestrator.ExtractDependencies(plan, "GET")
	if len(deps) != 3 {
		t.Fatalf("Expected 3 dependencies, got %d: %v", len(deps), deps)
	}

	expected := map[string]bool{"OperStatus": true, "Temperature": true, "Equipment": true}
	for _, d := range deps {
		if !expected[d] {
			t.Errorf("Unexpected dependency: %s", d)
		}
	}
	t.Logf("Dependencies: %v", deps)
}

func TestResolver_ExtractDependencies_MultiPreAction(t *testing.T) {
	// Verify ExtractDependencies deduplicates across multiple guards
	plan := &maestro.Plan{
		RunSpec: map[string]maestro.ActionSpec{
			"GET": {Guards: []maestro.Guard{
				{Check: "MUST_EQUAL", Key: "OperStatus", Params: mustJSON(maestro.MustEqualParams{})},
				{Check: "OVERWRITE", Key: "OperStatus", Params: mustJSON(maestro.OverwriteParams{})}, // duplicate
				{Check: "MUST_EXIST", Key: "Temperature", Params: mustJSON(maestro.MustExistParams{})},
			}},
		},
	}

	deps := orchestrator.ExtractDependencies(plan, "GET")
	if len(deps) != 2 {
		t.Fatalf("Expected 2 unique dependencies, got %d: %v", len(deps), deps)
	}
	t.Logf("Deduplicated dependencies: %v", deps)
}

// ============================================================
// Helper: byte array executor (simulates SNMP returning []byte)
// ============================================================

type byteArrayExecutor struct {
	value      []byte
	eng        *engine.Engine
	snmpValues map[string]any
}

func (e *byteArrayExecutor) Run(_ context.Context, plan *maestro.Plan, _ string, _ int, inputs map[string]any) (any, error) {
	if e.eng != nil && plan.Type == "OID_GEN" {
		state := maestro.NewState(inputs)
		result, err := e.eng.Execute(plan, state)
		if err != nil {
			return nil, err
		}
		if result.Status == maestro.StatusCompleted {
			resMap, ok := result.PrimitiveValue.(map[string]any)
			if ok {
				if oid, exists := resMap["oid"]; exists {
					oidStr := fmt.Sprintf("%v", oid)
					if val, found := e.snmpValues[oidStr]; found {
						return val, nil
					}
				}
			}
		}
	}
	return e.value, nil
}

func (e *byteArrayExecutor) RunBatch(_ context.Context, plans map[string]*maestro.Plan, _ string, _ int, inputs map[string]any) (map[string]any, error) {
	results := make(map[string]any)
	for prop, plan := range plans {
		val, err := e.Run(context.Background(), plan, "GET", 0, inputs)
		if err != nil {
			return nil, err
		}
		results[prop] = val
	}
	return results, nil
}

func (e *byteArrayExecutor) SupportsBatch() bool { return true }
func (e *byteArrayExecutor) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           "snmp",
		AllowedActions: []string{"GET", "SET", "WALK"},
		PlanIO: map[string]orchestrator.PlanTypeIO{
			"OID_GEN":        {DefaultAction: "GET"},
			"POST_PROC_SNMP": {DefaultAction: "EXECUTE", ContextInputs: []orchestrator.ContractInput{{Key: "snmp_value"}}},
		},
	}
}
