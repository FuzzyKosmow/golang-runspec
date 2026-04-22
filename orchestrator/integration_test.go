//go:build integration
// +build integration

package orchestrator_test

// Integration tests that load ALL plans from all.json and simulate
// the full orchestrator pipeline with a tracking runner that logs every call.
// Catches: double execution, missing post-processing, wrong OID generation, edge cases.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
	"os"
	"strings"
	"sync"
	"testing"
)

// allJSONPath points at the PARTIAL_VIEW_SETUP workspace's canonical plan
// dump. Override via GOLANG_RUNSPEC_PLANS for other layouts; tests skip if
// absent so the repo remains standalone-cloneable.
var allJSONPath = func() string {
	if env := os.Getenv("GOLANG_RUNSPEC_PLANS"); env != "" {
		return env
	}
	return "../../data/plans/all.json"
}()

// ============================================================
// planDB — loads all.json as a mock MongoDB
// ============================================================

type planDB struct {
	byScope   map[string]*maestro.Plan
	byPlanKey map[string]*maestro.Plan
	planCount int
}

func newPlanDB(t *testing.T) *planDB {
	t.Helper()
	data, err := os.ReadFile(allJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("all.json not available at %s — set GOLANG_RUNSPEC_PLANS to override", allJSONPath)
		}
		t.Fatalf("Failed to read all.json: %v", err)
	}

	var docs []struct {
		Name     string          `json:"name"`
		Type     string          `json:"type"`
		RunSpec json.RawMessage `json:"runspec"`
		Config   json.RawMessage `json:"config"`
		RawDef   json.RawMessage `json:"rawDefinition"`
	}
	if err := json.Unmarshal(data, &docs); err != nil {
		t.Fatalf("Failed to parse all.json: %v", err)
	}

	db := &planDB{
		byScope:   make(map[string]*maestro.Plan),
		byPlanKey: make(map[string]*maestro.Plan),
	}

	for _, doc := range docs {
		plan, err := n8n.Parse(doc.RawDef)
		if err != nil {
			continue
		}
		plan.Name = doc.Name
		plan.Type = doc.Type
		plan.Config = doc.Config
		parseRunSpec(plan, doc.RunSpec)

		var cfg struct {
			DeviceTypes []int  `json:"deviceTypes"`
			DeviceType  int    `json:"deviceType"`
			Property    string `json:"property"`
			PlanKey     string `json:"planKey"`
		}
		json.Unmarshal(doc.Config, &cfg)

		scopes := cfg.DeviceTypes
		if len(scopes) == 0 && cfg.DeviceType > 0 {
			scopes = []int{cfg.DeviceType}
		}

		for _, scope := range scopes {
			if cfg.Property != "" {
				db.byScope[fmt.Sprintf("%d:%s", scope, cfg.Property)] = plan
			}
			if cfg.PlanKey != "" {
				db.byPlanKey[fmt.Sprintf("%d:%s", scope, cfg.PlanKey)] = plan
			}
		}
		db.planCount++
	}

	t.Logf("[planDB] Loaded %d plans (%d scope entries, %d planKey entries)",
		db.planCount, len(db.byScope), len(db.byPlanKey))
	return db
}

func parseRunSpec(plan *maestro.Plan, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
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
	json.Unmarshal(raw, &rs)

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

func (db *planDB) GetPlan(_ context.Context, scope int, key string) (*maestro.Plan, error) {
	if p, ok := db.byScope[fmt.Sprintf("%d:%s", scope, key)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("plan not found: scope=%d key=%s", scope, key)
}

func (db *planDB) GetPlans(_ context.Context, scope int, keys []string) (map[string]*maestro.Plan, error) {
	result := make(map[string]*maestro.Plan)
	for _, key := range keys {
		if p, ok := db.byScope[fmt.Sprintf("%d:%s", scope, key)]; ok {
			result[key] = p
		}
	}
	return result, nil
}

func (db *planDB) GetPlanByKey(_ context.Context, scope int, planKey string) (*maestro.Plan, error) {
	if p, ok := db.byPlanKey[fmt.Sprintf("%d:%s", scope, planKey)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("plan not found: scope=%d planKey=%s", scope, planKey)
}

// ============================================================
// trackingRunner — logs every execution for optimization auditing
// ============================================================

type execCall struct {
	PlanName string
	PlanType string
	OID      string // generated OID (for OID_GEN plans)
}

type trackingRunner struct {
	eng        *engine.Engine
	snmpValues map[string]any
	mu         sync.Mutex
	calls      []execCall
}

func newTrackingRunner(snmpValues map[string]any) *trackingRunner {
	eng := engine.New()
	eng.RegisterStandardHandlers()
	return &trackingRunner{eng: eng, snmpValues: snmpValues}
}

func (r *trackingRunner) Run(_ context.Context, plan *maestro.Plan, _ string, _ int, inputs map[string]any) (any, error) {
	state := maestro.NewState(inputs)
	result, err := r.eng.Execute(plan, state)
	if err != nil {
		return nil, err
	}
	if result.Status != maestro.StatusCompleted {
		return nil, fmt.Errorf("plan %s did not complete: %s", plan.Name, result.Status)
	}

	call := execCall{PlanName: plan.Name, PlanType: plan.Type}

	if plan.Type == "OID_GEN" {
		resMap, ok := result.PrimitiveValue.(map[string]any)
		if ok {
			if oid, exists := resMap["oid"]; exists {
				oidStr := fmt.Sprintf("%v", oid)
				call.OID = oidStr
				r.mu.Lock()
				r.calls = append(r.calls, call)
				r.mu.Unlock()
				if val, found := r.snmpValues[oidStr]; found {
					return val, nil
				}
				return nil, fmt.Errorf("no mock SNMP value for OID: %s", oidStr)
			}
		}
	}

	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	return result.PrimitiveValue, nil
}

func (r *trackingRunner) RunMany(_ context.Context, _ string, _ int, invocations []orchestrator.Invocation) (map[string]any, error) {
	results := make(map[string]any)
	for _, inv := range invocations {
		val, err := r.Run(context.Background(), inv.Plan, "GET", 0, inv.Inputs)
		if err != nil {
			return nil, fmt.Errorf("batch exec %s: %w", inv.Key, err)
		}
		results[inv.Key] = val
	}
	return results, nil
}

func (r *trackingRunner) SupportsBatch() bool { return false }
func (r *trackingRunner) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           "snmp",
		AllowedActions: []string{"GET", "SET", "WALK"},
		PlanIO: map[string]orchestrator.PlanTypeIO{
			"OID_GEN":        {DefaultAction: "GET"},
			"POST_PROC_SNMP": {DefaultAction: "EXECUTE", ContextInputs: []orchestrator.ContractInput{{Key: "snmp_value"}}},
		},
	}
}

func (r *trackingRunner) printLog(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Logf("  [runner] %d plan executions:", len(r.calls))
	for i, c := range r.calls {
		if c.OID != "" {
			t.Logf("    %d. [%s] %s → OID %s", i+1, c.PlanType, c.PlanName, c.OID)
		} else {
			t.Logf("    %d. [%s] %s", i+1, c.PlanType, c.PlanName)
		}
	}
}

func (r *trackingRunner) assertCallCount(t *testing.T, expected int) {
	t.Helper()
	r.mu.Lock()
	actual := len(r.calls)
	r.mu.Unlock()
	if actual != expected {
		t.Errorf("  [OPTIMIZATION] Expected %d runner calls, got %d (possible double execution)", expected, actual)
	}
}

func (r *trackingRunner) assertNoDuplicates(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]int)
	for _, c := range r.calls {
		key := c.PlanName + "|" + c.OID
		seen[key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("  [OPTIMIZATION] Duplicate execution: %s ran %d times", key, count)
		}
	}
}

// ============================================================
// Test helpers
// ============================================================

func assertValue(t *testing.T, r orchestrator.Result, expected string) {
	t.Helper()
	if r.Status != orchestrator.StatusSuccess {
		t.Fatalf("    expected SUCCESS, got %s: %s", r.Status, r.Error)
	}
	valMap, ok := r.Value.(map[string]any)
	if ok {
		got := fmt.Sprintf("%v", valMap["value"])
		if got != expected {
			t.Errorf("    expected '%s', got '%s'", expected, got)
		}
	} else {
		got := fmt.Sprintf("%v", r.Value)
		if got != expected {
			t.Errorf("    expected '%s', got '%s'", expected, got)
		}
	}
}

func assertSkipped(t *testing.T, r orchestrator.Result) {
	t.Helper()
	if r.Status != orchestrator.StatusSkipped {
		t.Errorf("    expected SKIPPED, got %s: %s", r.Status, r.Error)
	}
}

// ============================================================
// D5 (ZTE C3XX) — Full Test Suite
// ============================================================

func TestSuite_D5(t *testing.T) {
	db := newPlanDB(t)

	t.Run("OperStatus_AllValues", func(t *testing.T) {
		cases := []struct {
			raw      string
			expected string
		}{
			{"0", "Offline"},
			{"1", "Online"},
			{"2", "Lawless"},
			{"3", "Unregister"},
			{"99", "Unknown"},
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{
					".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": tc.raw,
				})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 5,
					[]string{"OperStatus"},
					map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OperStatus"], tc.expected)
				runner.assertCallCount(t, 1) // OID_GEN only, post-proc via engine
				runner.printLog(t)
			})
		}
	})

	t.Run("OnStatus_AllValues", func(t *testing.T) {
		cases := []struct {
			raw      string
			expected string
		}{
			{"0", "power off"},
			{"1", "normal"},
			{"2", "los"},
			{"4", "lofi"},
			{"672", "All of the ONT has been deactivated"},
			{"999", "Unknown"},
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{
					".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": "1", // OperStatus Online (preflight)
					".1.3.6.1.4.1.13464.1.11.4.1.1.7.0.3.7": tc.raw,
				})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 5,
					[]string{"OnStatus"},
					map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OnStatus"], tc.expected)
				runner.printLog(t)
			})
		}
	})

	t.Run("RX_Online_GetsValue", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7":  "1",
			".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.3.7": "-22.5",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"RX"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if results["RX"].Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS, got %s: %s", results["RX"].Status, results["RX"].Error)
		}
		t.Logf("RX value: %v", results["RX"].Value)
		runner.printLog(t)
	})

	t.Run("RX_Offline_Skipped", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": "0",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"RX"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertSkipped(t, results["RX"])
		t.Logf("RX correctly skipped: %s", results["RX"].Error)
		runner.printLog(t)
	})

	t.Run("MultiProperty_SharedDep_NoDuplicateExec", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7":  "1",
			".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.3.7": "-22.5",
			".1.3.6.1.4.1.13464.1.11.4.1.1.23.0.3.7": "-8.3",
			".1.3.6.1.4.1.13464.1.11.4.1.1.24.0.3.7": "45",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"RX", "TX", "Temperature", "OperStatus"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, key := range []string{"RX", "TX", "Temperature", "OperStatus"} {
			if results[key].Status != orchestrator.StatusSuccess {
				t.Errorf("%s: expected SUCCESS, got %s: %s", key, results[key].Status, results[key].Error)
			}
			t.Logf("  %s: %v", key, results[key].Value)
		}
		runner.assertNoDuplicates(t)
		runner.printLog(t)
		// OperStatus should only be executed ONCE despite being a dependency of RX+TX
		t.Logf("  [optimization] OperStatus dep resolved once, shared across RX+TX")
	})

	t.Run("EdgeCase_ByteArray_SNMP", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7":  "1",
			".1.3.6.1.4.1.13464.1.11.4.1.1.22.0.3.7": []byte("-22.5"), // byte array
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"RX"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if results["RX"].Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS with []byte, got %s: %s", results["RX"].Status, results["RX"].Error)
		}
		t.Logf("RX from []byte: %v (type: %T)", results["RX"].Value, results["RX"].Value)
	})

	t.Run("EdgeCase_NonPrintableBytes_MAC", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": "1",
			".1.3.6.1.4.1.13464.1.11.4.1.1.2.0.3.7": []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"ONUMAC"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		t.Logf("ONUMAC from non-printable bytes: %v", results["ONUMAC"].Value)
	})
}

// ============================================================
// D8/9/12 (ZTE C6XX) — Full Test Suite
// ============================================================

func TestSuite_D8(t *testing.T) {
	db := newPlanDB(t)

	t.Run("OperStatus_AllValues", func(t *testing.T) {
		cases := []struct {
			raw      string
			expected string
		}{
			{"0", "Offline"},
			{"1", "Online"},
			{"2", "Lawless"},
			{"3", "Unregister"},
			{"42", "Unknown"},
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{
					".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.6.1.2.3": tc.raw,
				})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 8,
					[]string{"OperStatus"},
					map[string]any{"slotID": float64(1), "portID": float64(2), "onuID": float64(3)},
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OperStatus"], tc.expected)
			})
		}
	})

	t.Run("OnStatus_Online_ShortCircuit", func(t *testing.T) {
		// Override with our new fixture that has OVERWRITE short-circuit
		onStatusPlan := loadPlan(t, "prod_oid_gen_d8_9_12_onstatus.json")
		db.byScope["8:OnStatus"] = onStatusPlan

		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.6.1.2.3": "1", // OperStatus → Online
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 8,
			[]string{"OnStatus"},
			map[string]any{"slotID": float64(1), "portID": float64(2), "onuID": float64(3)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		r := results["OnStatus"]
		assertValue(t, r, "Normal")

		// Verify short-circuit: runner should NOT have executed OnStatus OID_GEN
		for _, c := range runner.calls {
			if strings.Contains(c.PlanName, "OnStatus") && c.PlanType == "OID_GEN" {
				t.Errorf("[OPTIMIZATION] OnStatus OID_GEN should NOT execute when short-circuited")
			}
		}
		runner.printLog(t)
		t.Logf("  [short-circuit] OperStatus=Online → returned 'Normal' without SNMP fetch")
	})

	t.Run("OnStatus_Offline_AllReasonCodes", func(t *testing.T) {
		onStatusPlan := loadPlan(t, "prod_oid_gen_d8_9_12_onstatus.json")
		db.byScope["8:OnStatus"] = onStatusPlan

		cases := []struct {
			raw      string
			expected string
		}{
			{"0", "None"},
			{"1", "Deactive"},
			{"2", "Timeout"},
			{"3", "SFi"},
			{"4", "TIWi"},
			{"5", "PWD Auth Failed"},
			{"6", "LOSi"},
			{"7", "LOS"},
			{"8", "LOKi"},
			{"9", "Rerange Failed"},
			{"10", "LOFi"},
			{"11", "PowerOff"},
			{"99", "Unknown"},
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{
					".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.6.1.2.3":  "0",    // OperStatus → Offline
					".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.12.1.2.3": tc.raw, // OnStatus raw
				})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 8,
					[]string{"OnStatus"},
					map[string]any{"slotID": float64(1), "portID": float64(2), "onuID": float64(3)},
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OnStatus"], tc.expected)
			})
		}
	})

	t.Run("MultiProperty_AllOnline", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.6.1.2.3":  "1",
			".1.3.6.1.4.1.13464.1.14.2.4.1.4.1.5.1.2.3":  "-25.1",
			".1.3.6.1.4.1.13464.1.14.2.4.1.4.1.6.1.2.3":  "-7.2",
			".1.3.6.1.4.1.13464.1.14.2.4.1.4.1.8.1.2.3":  "43",
			".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.7.1.2.3":  "1200",
			".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.5.1.2.3":  "ZXA10-F660",
			".1.3.6.1.4.1.13464.1.14.2.4.1.2.1.6.1.2.3":  "V5.0",
			".1.3.6.1.4.1.13464.1.14.2.4.1.2.1.7.1.2.3":  "3.0",
			".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.8.1.2.3":  "ZTEG12345678",
			".1.3.6.1.4.1.13464.1.14.2.4.1.1.1.12.1.2.3": "1",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 8,
			[]string{"OperStatus", "RX", "TX", "Temperature", "CableLength", "ONUModel", "Equipment", "Firmware", "SerialNumber"},
			map[string]any{"slotID": float64(1), "portID": float64(2), "onuID": float64(3)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		for _, key := range []string{"OperStatus", "RX", "TX", "Temperature", "CableLength", "ONUModel", "Equipment", "Firmware", "SerialNumber"} {
			r := results[key]
			if r.Status != orchestrator.StatusSuccess {
				t.Errorf("  %s: expected SUCCESS, got %s: %s", key, r.Status, r.Error)
			} else {
				t.Logf("  %s: %v", key, r.Value)
			}
		}
		runner.assertNoDuplicates(t)
		runner.printLog(t)
	})
}

// ============================================================
// D13 (ZTE C6XX-v2) — uses ifindex, no OperStatus
// ============================================================

func TestSuite_D13(t *testing.T) {
	db := newPlanDB(t)

	t.Run("OnStatus_AllValues", func(t *testing.T) {
		// D13 OnStatus uses ifindex (285278470 for slot1 port6)
		cases := []struct {
			raw      string
			expected string
		}{
			{"1", "logging"},
			{"2", "los"},
			{"3", "syncMib"},
			{"4", "working"},
			{"99", "unknown"}, // D13 plan uses lowercase default
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{
					".1.3.6.1.4.1.3902.1082.500.10.2.3.8.1.4.285278470.7": tc.raw,
				})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 13,
					[]string{"OnStatus"},
					map[string]any{"slotID": float64(1), "portID": float64(6), "onuID": float64(7)},
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OnStatus"], tc.expected)
			})
		}
	})

	t.Run("RX_WithOnStatusPreflight_Working", func(t *testing.T) {
		// D13 RX has MUST_EQUAL preflight on OnStatus (expects "working")
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.3902.1082.500.10.2.3.8.1.4.285278470.7":   "4",     // OnStatus → working
			".1.3.6.1.4.1.3902.1082.500.20.2.2.2.1.10.285278470.7.1": "16000", // RX raw
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 13,
			[]string{"RX"},
			map[string]any{"slotID": float64(1), "portID": float64(6), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["RX"]
		if r.Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS, got %s: %s", r.Status, r.Error)
		}
		// D13 RX has POST_PROC: val*0.002-30 → 16000*0.002-30 = 2.0
		t.Logf("D13 RX (raw=16000, OnStatus=working): %v", r.Value)
		runner.printLog(t)
	})

	t.Run("RX_OnStatusNotWorking_Skipped", func(t *testing.T) {
		// D13 RX skipped when OnStatus != "working"
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.3902.1082.500.10.2.3.8.1.4.285278470.7": "2", // OnStatus → los
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 13,
			[]string{"RX"},
			map[string]any{"slotID": float64(1), "portID": float64(6), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertSkipped(t, results["RX"])
		t.Logf("D13 RX correctly skipped (OnStatus=los): %s", results["RX"].Error)
		runner.printLog(t)
	})
}

// ============================================================
// Edge Cases & Optimization Tests
// ============================================================

func TestSuite_EdgeCases(t *testing.T) {
	db := newPlanDB(t)

	t.Run("EmptyKeys_NoError", func(t *testing.T) {
		runner := newTrackingRunner(nil)
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5, []string{}, map[string]any{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("Expected empty results, got %d", len(results))
		}
	})

	t.Run("UnknownProperty_Error", func(t *testing.T) {
		runner := newTrackingRunner(nil)
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"NonExistentProperty"},
			map[string]any{"portID": float64(1), "onuID": float64(1)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["NonExistentProperty"]
		if r.Status != orchestrator.StatusError {
			t.Errorf("Expected ERROR for unknown property, got %s", r.Status)
		}
		t.Logf("Unknown property: %s", r.Error)
	})

	t.Run("SNMP_Timeout_Error", func(t *testing.T) {
		// No mock value → simulates SNMP timeout/error
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": "1", // OperStatus works
			// RX OID deliberately missing → error
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"RX"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["RX"]
		if r.Status != orchestrator.StatusError {
			t.Errorf("Expected ERROR (no SNMP value), got %s", r.Status)
		}
		t.Logf("SNMP error caught: %s", r.Error)
	})

	t.Run("NumericString_SNMP", func(t *testing.T) {
		// SNMP returns "1" as string vs int vs float — all should work
		for _, val := range []any{"1", 1, 1.0, float64(1), int64(1)} {
			runner := newTrackingRunner(map[string]any{
				".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": val,
			})
			orch := orchestrator.NewOrchestrator(db, runner, nil)
			results, err := orch.Run(context.Background(), "GET", 5,
				[]string{"OperStatus"},
				map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
			)
			if err != nil {
				t.Fatalf("Run with %T(%v): %v", val, val, err)
			}
			assertValue(t, results["OperStatus"], "Online")
		}
		t.Logf("All numeric types handled correctly")
	})

	t.Run("Gate_BlocksUnallowed", func(t *testing.T) {
		runner := newTrackingRunner(map[string]any{
			".1.3.6.1.4.1.13464.1.11.4.1.1.3.0.3.7": "1",
		})
		gate := &mockGate{allowed: map[int]map[string]bool{
			5: {"OperStatus": true}, // RX not allowed
		}}
		orch := orchestrator.NewOrchestrator(db, runner, gate)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"OperStatus", "RX"},
			map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)},
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if results["OperStatus"].Status != orchestrator.StatusSuccess {
			t.Errorf("OperStatus should pass gate")
		}
		if results["RX"].Status != orchestrator.StatusUnsupported {
			t.Errorf("RX should be blocked by gate, got %s", results["RX"].Status)
		}
		runner.assertCallCount(t, 1) // Only OperStatus executed
		runner.printLog(t)
	})
}

// ============================================================
// discoverOID — runs an OID_GEN plan through the engine to find the generated OID
// ============================================================

func discoverOID(t *testing.T, db *planDB, scope int, property string, inputs map[string]any) string {
	t.Helper()
	plan, err := db.GetPlan(context.Background(), scope, property)
	if err != nil {
		t.Fatalf("discoverOID: plan not found for scope=%d property=%s: %v", scope, property, err)
	}
	eng := engine.New()
	eng.RegisterStandardHandlers()
	state := maestro.NewState(inputs)
	result, err := eng.Execute(plan, state)
	if err != nil {
		t.Fatalf("discoverOID: engine execute failed for %s: %v", property, err)
	}
	if result.Status != maestro.StatusCompleted {
		t.Fatalf("discoverOID: plan did not complete for %s: %s", property, result.Status)
	}
	resMap, ok := result.PrimitiveValue.(map[string]any)
	if !ok {
		t.Fatalf("discoverOID: expected map result for %s, got %T", property, result.PrimitiveValue)
	}
	oid, exists := resMap["oid"]
	if !exists {
		t.Fatalf("discoverOID: no 'oid' key in result for %s: %v", property, resMap)
	}
	return fmt.Sprintf("%v", oid)
}

// ============================================================
// D14 (Huawei MA58XX) — Full Test Suite
// ============================================================

func TestSuite_D14(t *testing.T) {
	db := newPlanDB(t)
	d14Inputs := map[string]any{"slotID": float64(0), "portID": float64(0), "onuID": float64(1)}

	t.Run("OperStatus_AllValues", func(t *testing.T) {
		oid := discoverOID(t, db, 14, "OperStatus", d14Inputs)
		t.Logf("D14 OperStatus OID: %s", oid)

		cases := []struct {
			raw      string
			expected string
		}{
			{"1", "Online"},
			{"2", "Down"},
			{"99", "Unknown"},
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{oid: tc.raw})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 14,
					[]string{"OperStatus"},
					d14Inputs,
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OperStatus"], tc.expected)
				runner.assertCallCount(t, 1)
				runner.printLog(t)
			})
		}
	})

	t.Run("OnStatus_AllValues", func(t *testing.T) {
		oid := discoverOID(t, db, 14, "OnStatus", d14Inputs)
		t.Logf("D14 OnStatus OID: %s", oid)

		cases := []struct {
			raw      string
			expected string
		}{
			{"1", "initialization"},
			{"2", "normal"},
			{"3", "failed"},
			{"4", "noresume"},
			{"5", "config"},
			{"-1", "invalid"},
			{"99", "invalid"},
		}
		for _, tc := range cases {
			t.Run(tc.expected+"_raw_"+tc.raw, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{oid: tc.raw})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 14,
					[]string{"OnStatus"},
					d14Inputs,
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OnStatus"], tc.expected)
				runner.printLog(t)
			})
		}
	})

	t.Run("RX_Online_GetsValue", func(t *testing.T) {
		operOID := discoverOID(t, db, 14, "OperStatus", d14Inputs)
		rxOID := discoverOID(t, db, 14, "RX", d14Inputs)
		t.Logf("D14 OperStatus OID: %s, RX OID: %s", operOID, rxOID)

		runner := newTrackingRunner(map[string]any{
			operOID: "1",      // OperStatus → Online (preflight)
			rxOID:   "-2250",  // RX raw (will be /100 → -22.50)
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 14,
			[]string{"RX"},
			d14Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["RX"]
		if r.Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS, got %s: %s", r.Status, r.Error)
		}
		t.Logf("D14 RX (raw=-2250, /100): %v", r.Value)
		runner.printLog(t)
	})

	t.Run("RX_Offline_Skipped", func(t *testing.T) {
		operOID := discoverOID(t, db, 14, "OperStatus", d14Inputs)

		runner := newTrackingRunner(map[string]any{
			operOID: "2", // OperStatus → Down
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 14,
			[]string{"RX"},
			d14Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertSkipped(t, results["RX"])
		t.Logf("D14 RX correctly skipped (device down): %s", results["RX"].Error)
		runner.printLog(t)
	})

	t.Run("MultiProperty_AllOnline", func(t *testing.T) {
		operOID := discoverOID(t, db, 14, "OperStatus", d14Inputs)
		rxOID := discoverOID(t, db, 14, "RX", d14Inputs)
		txOID := discoverOID(t, db, 14, "TX", d14Inputs)
		onStatusOID := discoverOID(t, db, 14, "OnStatus", d14Inputs)
		cableLenOID := discoverOID(t, db, 14, "CableLength", d14Inputs)
		equipOID := discoverOID(t, db, 14, "Equipment", d14Inputs)
		fwOID := discoverOID(t, db, 14, "Firmware", d14Inputs)
		serialOID := discoverOID(t, db, 14, "SerialNumber", d14Inputs)
		modelOID := discoverOID(t, db, 14, "ONUModel", d14Inputs)

		runner := newTrackingRunner(map[string]any{
			operOID:     "1",
			rxOID:       "-2250",
			txOID:       "-150",
			onStatusOID: "2",
			cableLenOID: "1200",
			equipOID:    "HG8546M",
			fwOID:       "V300R019C10",
			serialOID:   "48575443DEADBEEF",
			modelOID:    "EchoLife HG8546M",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		props := []string{"OperStatus", "RX", "TX", "OnStatus", "CableLength", "Equipment", "Firmware", "SerialNumber", "ONUModel"}
		results, err := orch.Run(context.Background(), "GET", 14, props, d14Inputs)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		for _, key := range props {
			r := results[key]
			if r.Status != orchestrator.StatusSuccess {
				t.Errorf("  %s: expected SUCCESS, got %s: %s", key, r.Status, r.Error)
			} else {
				t.Logf("  %s: %v", key, r.Value)
			}
		}
		runner.assertNoDuplicates(t)
		runner.printLog(t)
	})
}

// ============================================================
// D17 (Nokia) — Full Test Suite
// ============================================================

func TestSuite_D17(t *testing.T) {
	db := newPlanDB(t)
	d17Inputs := map[string]any{"slotID": float64(1), "portID": float64(1), "onuID": float64(3)}

	t.Run("OperStatus_AllValues", func(t *testing.T) {
		oid := discoverOID(t, db, 17, "OperStatus", d17Inputs)
		t.Logf("D17 OperStatus OID: %s", oid)

		cases := []struct {
			raw      string
			expected string
		}{
			{"1", "Online"},
			{"2", "Offline"},
			{"0", "Offline"},
			{"99", "Offline"},
		}
		for _, tc := range cases {
			t.Run(tc.expected+"_raw_"+tc.raw, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{oid: tc.raw})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 17,
					[]string{"OperStatus"},
					d17Inputs,
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OperStatus"], tc.expected)
				runner.assertCallCount(t, 1)
				runner.printLog(t)
			})
		}
	})

	t.Run("OnStatus_AllValues", func(t *testing.T) {
		oid := discoverOID(t, db, 17, "OnStatus", d17Inputs)
		t.Logf("D17 OnStatus OID: %s", oid)

		cases := []struct {
			raw      string
			expected string
		}{
			{"0", "los"},
			{"1", "Normal"},
			{"2", "Power off"},
			{"3", "Offline"},
			{"4", "OLT PON port down"},
			{"99", "offline"},
		}
		for _, tc := range cases {
			t.Run(tc.expected+"_raw_"+tc.raw, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{oid: tc.raw})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 17,
					[]string{"OnStatus"},
					d17Inputs,
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["OnStatus"], tc.expected)
				runner.printLog(t)
			})
		}
	})

	t.Run("RX_Online_GetsValue", func(t *testing.T) {
		operOID := discoverOID(t, db, 17, "OperStatus", d17Inputs)
		rxOID := discoverOID(t, db, 17, "RX", d17Inputs)
		t.Logf("D17 OperStatus OID: %s, RX OID: %s", operOID, rxOID)

		runner := newTrackingRunner(map[string]any{
			operOID: "1",     // OperStatus → Online
			rxOID:   "-1850", // RX raw (will be /100 → -18.50)
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 17,
			[]string{"RX"},
			d17Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["RX"]
		if r.Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS, got %s: %s", r.Status, r.Error)
		}
		t.Logf("D17 RX (raw=-1850, /100): %v", r.Value)
		runner.printLog(t)
	})

	t.Run("RX_Offline_Skipped", func(t *testing.T) {
		operOID := discoverOID(t, db, 17, "OperStatus", d17Inputs)

		runner := newTrackingRunner(map[string]any{
			operOID: "0", // OperStatus → Offline
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 17,
			[]string{"RX"},
			d17Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertSkipped(t, results["RX"])
		t.Logf("D17 RX correctly skipped (device offline): %s", results["RX"].Error)
		runner.printLog(t)
	})

	t.Run("MultiProperty_AllOnline", func(t *testing.T) {
		operOID := discoverOID(t, db, 17, "OperStatus", d17Inputs)
		rxOID := discoverOID(t, db, 17, "RX", d17Inputs)
		txOID := discoverOID(t, db, 17, "TX", d17Inputs)
		onStatusOID := discoverOID(t, db, 17, "OnStatus", d17Inputs)
		cableLenOID := discoverOID(t, db, 17, "CableLength", d17Inputs)
		equipOID := discoverOID(t, db, 17, "Equipment", d17Inputs)
		fwOID := discoverOID(t, db, 17, "Firmware", d17Inputs)
		serialOID := discoverOID(t, db, 17, "SerialNumber", d17Inputs)
		modelOID := discoverOID(t, db, 17, "ONUModel", d17Inputs)

		runner := newTrackingRunner(map[string]any{
			operOID:     "1",
			rxOID:       "-1850",
			txOID:       "-200",
			onStatusOID: "1",
			cableLenOID: "500",
			equipOID:    "G-240W-A",
			fwOID:       "3FE46606AFGA92",
			serialOID:   "ALCL12345678",
			modelOID:    "G-240W-A",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		props := []string{"OperStatus", "RX", "TX", "OnStatus", "CableLength", "Equipment", "Firmware", "SerialNumber", "ONUModel"}
		results, err := orch.Run(context.Background(), "GET", 17, props, d17Inputs)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		for _, key := range props {
			r := results[key]
			if r.Status != orchestrator.StatusSuccess {
				t.Errorf("  %s: expected SUCCESS, got %s: %s", key, r.Status, r.Error)
			} else {
				t.Logf("  %s: %v", key, r.Value)
			}
		}
		runner.assertNoDuplicates(t)
		runner.printLog(t)
	})

	t.Run("StringProperties_NoPostProc", func(t *testing.T) {
		// D17 Equipment, ONUModel, Firmware, CableLength have no post-processing
		equipOID := discoverOID(t, db, 17, "Equipment", d17Inputs)
		modelOID := discoverOID(t, db, 17, "ONUModel", d17Inputs)
		fwOID := discoverOID(t, db, 17, "Firmware", d17Inputs)

		runner := newTrackingRunner(map[string]any{
			equipOID: "G-240W-A",
			modelOID: "G-240W-A",
			fwOID:    "3FE46606AFGA92",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 17,
			[]string{"Equipment", "ONUModel", "Firmware"},
			d17Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		// These properties have no preActions and no postActions — raw string pass-through
		for _, key := range []string{"Equipment", "ONUModel", "Firmware"} {
			r := results[key]
			if r.Status != orchestrator.StatusSuccess {
				t.Errorf("  %s: expected SUCCESS, got %s: %s", key, r.Status, r.Error)
			} else {
				t.Logf("  %s: %v", key, r.Value)
			}
		}
		runner.assertCallCount(t, 3) // No preflight dependencies needed
		runner.printLog(t)
	})
}

// ============================================================
// D5 Expanded Coverage — AdminStatus, Equipment, Firmware, ONUModel
// ============================================================

func TestSuite_D5_Expanded(t *testing.T) {
	db := newPlanDB(t)
	d5Inputs := map[string]any{"slotID": float64(0), "portID": float64(3), "onuID": float64(7)}

	t.Run("AdminStatus_AllValues", func(t *testing.T) {
		oid := discoverOID(t, db, 5, "AdminStatus", d5Inputs)
		t.Logf("D5 AdminStatus OID: %s", oid)

		cases := []struct {
			raw      string
			expected string
		}{
			{"0", "Disable"},
			{"1", "Enable"},
			{"99", "Lawless"},
		}
		for _, tc := range cases {
			t.Run(tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{oid: tc.raw})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 5,
					[]string{"AdminStatus"},
					d5Inputs,
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["AdminStatus"], tc.expected)
				runner.assertCallCount(t, 1) // OID_GEN only, no preflight
				runner.printLog(t)
			})
		}
	})

	t.Run("Equipment_Online_StringPassThrough", func(t *testing.T) {
		operOID := discoverOID(t, db, 5, "OperStatus", d5Inputs)
		equipOID := discoverOID(t, db, 5, "Equipment", d5Inputs)
		t.Logf("D5 Equipment OID: %s (preflight OperStatus OID: %s)", equipOID, operOID)

		runner := newTrackingRunner(map[string]any{
			operOID:  "1",        // Online
			equipOID: "ZTE-C320", // raw string pass-through (no post-proc)
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"Equipment"},
			d5Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["Equipment"]
		if r.Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS, got %s: %s", r.Status, r.Error)
		}
		t.Logf("D5 Equipment: %v", r.Value)
		runner.printLog(t)
	})

	t.Run("Equipment_Offline_Skipped", func(t *testing.T) {
		operOID := discoverOID(t, db, 5, "OperStatus", d5Inputs)

		runner := newTrackingRunner(map[string]any{
			operOID: "0", // Offline
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"Equipment"},
			d5Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertSkipped(t, results["Equipment"])
		t.Logf("D5 Equipment correctly skipped (offline): %s", results["Equipment"].Error)
	})

	t.Run("Firmware_Online_StringPassThrough", func(t *testing.T) {
		operOID := discoverOID(t, db, 5, "OperStatus", d5Inputs)
		fwOID := discoverOID(t, db, 5, "Firmware", d5Inputs)
		t.Logf("D5 Firmware OID: %s", fwOID)

		runner := newTrackingRunner(map[string]any{
			operOID: "1",
			fwOID:   "V5.0.10P3T2",
		})
		orch := orchestrator.NewOrchestrator(db, runner, nil)
		results, err := orch.Run(context.Background(), "GET", 5,
			[]string{"Firmware"},
			d5Inputs,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results["Firmware"]
		if r.Status != orchestrator.StatusSuccess {
			t.Fatalf("Expected SUCCESS, got %s: %s", r.Status, r.Error)
		}
		t.Logf("D5 Firmware: %v", r.Value)
		runner.printLog(t)
	})

	t.Run("ONUModel_PostProc", func(t *testing.T) {
		// D5 ONUModel has POST_PROC: maps numeric codes to model names
		oid := discoverOID(t, db, 5, "ONUModel", d5Inputs)
		t.Logf("D5 ONUModel OID: %s", oid)

		cases := []struct {
			raw      string
			expected string
		}{
			{"9", "G-93RG"},       // mapped value
			{"42", "42"},          // default: pass-through of raw value
		}
		for _, tc := range cases {
			t.Run("raw_"+tc.raw+"_expect_"+tc.expected, func(t *testing.T) {
				runner := newTrackingRunner(map[string]any{oid: tc.raw})
				orch := orchestrator.NewOrchestrator(db, runner, nil)
				results, err := orch.Run(context.Background(), "GET", 5,
					[]string{"ONUModel"},
					d5Inputs,
				)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				assertValue(t, results["ONUModel"], tc.expected)
				runner.printLog(t)
			})
		}
	})
}
