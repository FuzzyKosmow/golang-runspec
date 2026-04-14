//go:build integration
// +build integration

package engine_test

import (
	"encoding/json"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
	"os"
	"path/filepath"
	"testing"
)

// testPlanDir points to the shared test fixtures.
// These are the same files used by the Node.js validator.
const testPlanDir = "../../../../SampleExecutionPlan-StoredInMongo/tests/fixtures"

// loadFixture loads a v2 plan fixture, extracts rawDefinition, and parses it.
func loadFixture(t *testing.T, filename string) *maestro.Plan {
	t.Helper()
	path := filepath.Join(testPlanDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read fixture %s: %v", filename, err)
	}

	// v2 fixture has top-level {pipeline, config, rawDefinition, ...}
	// We need rawDefinition for the engine, pipeline/config for orchestrator tests.
	var doc struct {
		RawDefinition json.RawMessage `json:"rawDefinition"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Failed to parse fixture %s: %v", filename, err)
	}

	plan, err := n8n.Parse(doc.RawDefinition)
	if err != nil {
		t.Fatalf("Failed to parse rawDefinition from %s: %v", filename, err)
	}
	return plan
}

// loadFixtureFull loads the full v3 document (runspec + config + rawDefinition).
type V3Document struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Name          string                    `json:"name"`
	Type          string                    `json:"type"`
	RunSpec       map[string]V3ActionSpec   `json:"runspec"`
	Config        json.RawMessage           `json:"config"`
	RawDefinition json.RawMessage           `json:"rawDefinition"`
}

type V3ActionSpec struct {
	Guards []V3Guard `json:"guards"`
	Chains []V3Chain `json:"chains"`
}

type V3Guard struct {
	Check  string          `json:"check"`
	Key    string          `json:"key"`
	Params json.RawMessage `json:"params,omitempty"`
}

type V3Chain struct {
	Do     string          `json:"do"`
	Params json.RawMessage `json:"params,omitempty"`
}

// guardParams extracts typed params from a guard.
func (g V3Guard) mustEqualParams() (expect []string, onFail, message string) {
	var p struct {
		Expect  []string `json:"expect"`
		OnFail  string   `json:"onFail"`
		Message string   `json:"message"`
	}
	json.Unmarshal(g.Params, &p)
	return p.Expect, p.OnFail, p.Message
}

func (g V3Guard) overwriteParams() (as string) {
	var p struct{ As string `json:"as"` }
	json.Unmarshal(g.Params, &p)
	return p.As
}

func (c V3Chain) planKey() string {
	var p struct{ PlanKey string `json:"planKey"` }
	json.Unmarshal(c.Params, &p)
	return p.PlanKey
}

func loadFullFixture(t *testing.T, filename string) *V3Document {
	t.Helper()
	path := filepath.Join(testPlanDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read fixture %s: %v", filename, err)
	}
	var doc V3Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Failed to parse fixture %s: %v", filename, err)
	}
	return &doc
}

func newEngine() *engine.Engine {
	eng := engine.New()
	eng.RegisterStandardHandlers()
	return eng
}

// ============================================================
// Schema Validation Tests (v2 document structure)
// ============================================================

func TestV3Schema_MustEqual(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_must_equal.json")

	if doc.SchemaVersion != 3 {
		t.Errorf("schemaVersion: got %d, want 3", doc.SchemaVersion)
	}
	if doc.Type != "OID_GEN" {
		t.Errorf("type: got %s, want OID_GEN", doc.Type)
	}
	getSpec, ok := doc.RunSpec["GET"]
	if !ok {
		t.Fatal("missing runspec.GET")
	}
	if len(getSpec.Guards) != 1 {
		t.Fatalf("guards: got %d, want 1", len(getSpec.Guards))
	}

	g := getSpec.Guards[0]
	if g.Check != "MUST_EQUAL" {
		t.Errorf("check: got %s, want MUST_EQUAL", g.Check)
	}
	if g.Key != "OperStatus" {
		t.Errorf("key: got %s, want OperStatus", g.Key)
	}
	expect, onFail, message := g.mustEqualParams()
	if len(expect) != 1 || expect[0] != "Online" {
		t.Errorf("expect: got %v, want [Online]", expect)
	}
	if onFail != "SKIP" {
		t.Errorf("onFail: got %s, want SKIP", onFail)
	}
	if message == "" {
		t.Error("message should not be empty")
	}
}

func TestV3Schema_Overwrite(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_overwrite.json")

	getSpec := doc.RunSpec["GET"]
	if len(getSpec.Guards) != 1 {
		t.Fatalf("guards: got %d, want 1", len(getSpec.Guards))
	}

	g := getSpec.Guards[0]
	if g.Check != "OVERWRITE" {
		t.Errorf("check: got %s, want OVERWRITE", g.Check)
	}
	as := g.overwriteParams()
	if as != "operStatus" {
		t.Errorf("as: got %s, want operStatus", as)
	}

	if len(getSpec.Chains) != 1 {
		t.Fatalf("chains: got %d, want 1", len(getSpec.Chains))
	}
	chain := getSpec.Chains[0]
	if chain.Do != "RUN_PLAN" {
		t.Errorf("chain.do: got %s, want RUN_PLAN", chain.Do)
	}
	if chain.planKey() == "" {
		t.Error("chain.planKey should not be empty")
	}
}

func TestV3Schema_NoActions(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_no_actions.json")
	getSpec := doc.RunSpec["GET"]
	if len(getSpec.Guards) != 0 {
		t.Errorf("guards: got %d, want 0", len(getSpec.Guards))
	}
	if len(getSpec.Chains) != 0 {
		t.Errorf("chains: got %d, want 0", len(getSpec.Chains))
	}
}

func TestV3Schema_MultiExpect(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_multi_expect.json")
	g := doc.RunSpec["GET"].Guards[0]
	expect, _, _ := g.mustEqualParams()
	if len(expect) != 4 {
		t.Errorf("expect: got %d values, want 4", len(expect))
	}
	expected := []string{"logging", "los", "syncMib", "working"}
	for i, v := range expected {
		if expect[i] != v {
			t.Errorf("expect[%d]: got %s, want %s", i, expect[i], v)
		}
	}
}

// ============================================================
// Engine Execution Tests (rawDefinition through actual engine)
// ============================================================

func TestEngine_SimplePlan(t *testing.T) {
	plan := loadFixture(t, "v2_plan_no_actions.json")
	eng := newEngine()

	inputs := map[string]any{"portID": float64(5)}
	state := maestro.NewState(inputs)

	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Engine execution failed: %v", err)
	}
	if result.Status != maestro.StatusCompleted {
		t.Fatalf("Expected COMPLETED, got %s", result.Status)
	}

	resMap, ok := result.PrimitiveValue.(map[string]any)
	if !ok {
		t.Fatalf("Expected map result, got %T: %v", result.PrimitiveValue, result.PrimitiveValue)
	}
	if resMap["value"] != float64(5) {
		t.Errorf("Expected value=5, got %v", resMap["value"])
	}
}

func TestEngine_OnStatusOverwrite_Online(t *testing.T) {
	// D8/9/12 OnStatus pattern:
	// Input: snmp_value=7 (stale LOS), operStatus="Online" (injected by OVERWRITE)
	// Expected: If branch → "Normal" (ignores stale value)
	plan := loadFixture(t, "v2_plan_overwrite.json")
	eng := newEngine()

	inputs := map[string]any{
		"snmp_value":  float64(7),
		"operStatus": "Online",
	}
	state := maestro.NewState(inputs)

	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Engine execution failed: %v", err)
	}
	if result.Status != maestro.StatusCompleted {
		t.Fatalf("Expected COMPLETED, got %s", result.Status)
	}

	resMap, ok := result.PrimitiveValue.(map[string]any)
	if !ok {
		t.Fatalf("Expected map result, got %T", result.PrimitiveValue)
	}
	if resMap["value"] != "Normal" {
		t.Errorf("Expected 'Normal' (online override), got '%v'", resMap["value"])
	}
}

func TestEngine_OnStatusOverwrite_Offline(t *testing.T) {
	// D8/9/12 OnStatus pattern:
	// Input: snmp_value=7 (LOS), operStatus="Offline"
	// Expected: SwitchMap branch → "LOS"
	plan := loadFixture(t, "v2_plan_overwrite.json")
	eng := newEngine()

	inputs := map[string]any{
		"snmp_value":  float64(7),
		"operStatus": "Offline",
	}
	state := maestro.NewState(inputs)

	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Engine execution failed: %v", err)
	}

	resMap, ok := result.PrimitiveValue.(map[string]any)
	if !ok {
		t.Fatalf("Expected map result, got %T", result.PrimitiveValue)
	}
	if resMap["value"] != "LOS" {
		t.Errorf("Expected 'LOS' (offline SwitchMap), got '%v'", resMap["value"])
	}
}

func TestEngine_OnStatusOverwrite_OfflineDeactive(t *testing.T) {
	// snmp_value=1 (Deactive), operStatus="Offline"
	plan := loadFixture(t, "v2_plan_overwrite.json")
	eng := newEngine()

	inputs := map[string]any{
		"snmp_value":  float64(1),
		"operStatus": "Offline",
	}
	state := maestro.NewState(inputs)

	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Engine execution failed: %v", err)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	if resMap["value"] != "Deactive" {
		t.Errorf("Expected 'Deactive', got '%v'", resMap["value"])
	}
}

func TestEngine_OnStatusOverwrite_OfflineUnknown(t *testing.T) {
	// snmp_value=99 (not in SwitchMap), operStatus="Offline"
	// Expected: default → "Unknown"
	plan := loadFixture(t, "v2_plan_overwrite.json")
	eng := newEngine()

	inputs := map[string]any{
		"snmp_value":  float64(99),
		"operStatus": "Offline",
	}
	state := maestro.NewState(inputs)

	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("Engine execution failed: %v", err)
	}

	resMap := result.PrimitiveValue.(map[string]any)
	if resMap["value"] != "Unknown" {
		t.Errorf("Expected 'Unknown' (default), got '%v'", resMap["value"])
	}
}

// ============================================================
// Guard Logic Tests (simulating orchestrator decisions)
// ============================================================

func TestGuard_MustEqual_Match(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_must_equal.json")
	g := doc.RunSpec["GET"].Guards[0]
	expect, _, _ := g.mustEqualParams()

	resolved := "Online"
	match := false
	for _, exp := range expect {
		if resolved == exp {
			match = true
			break
		}
	}
	if !match {
		t.Error("MUST_EQUAL should match: resolved='Online', expect=['Online']")
	}
}

func TestGuard_MustEqual_NoMatch(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_must_equal.json")
	g := doc.RunSpec["GET"].Guards[0]
	expect, onFail, message := g.mustEqualParams()

	resolved := "Offline"
	match := false
	for _, exp := range expect {
		if resolved == exp {
			match = true
			break
		}
	}
	if match {
		t.Error("MUST_EQUAL should NOT match: resolved='Offline', expect=['Online']")
	}
	if onFail != "SKIP" {
		t.Errorf("onFail should be SKIP, got %s", onFail)
	}
	if message == "" {
		t.Error("message should provide user-facing reason for SKIP")
	}
}

func TestGuard_MultiExpect_Match(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_multi_expect.json")
	g := doc.RunSpec["GET"].Guards[0]
	expect, _, _ := g.mustEqualParams()

	resolved := "working"
	match := false
	for _, exp := range expect {
		if resolved == exp {
			match = true
			break
		}
	}
	if !match {
		t.Error("MUST_EQUAL should match: resolved='working', expect contains 'working'")
	}
}

func TestGuard_MultiExpect_NoMatch(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_multi_expect.json")
	g := doc.RunSpec["GET"].Guards[0]
	expect, _, _ := g.mustEqualParams()

	resolved := "offline"
	match := false
	for _, exp := range expect {
		if resolved == exp {
			match = true
			break
		}
	}
	if match {
		t.Error("MUST_EQUAL should NOT match: 'offline' not in expect list")
	}
}

func TestGuard_Overwrite_InjectionMap(t *testing.T) {
	doc := loadFullFixture(t, "v2_plan_overwrite.json")
	g := doc.RunSpec["GET"].Guards[0]

	if g.Check != "OVERWRITE" {
		t.Fatalf("Expected OVERWRITE check, got %s", g.Check)
	}

	as := g.overwriteParams()
	injectionMap := make(map[string]any)
	resolvedValue := "Online"

	if as != "" {
		injectionMap[as] = resolvedValue
	}

	if injectionMap["operStatus"] != "Online" {
		t.Errorf("injectionMap['operStatus'] = %v, want 'Online'", injectionMap["operStatus"])
	}
}
