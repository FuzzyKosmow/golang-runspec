package orchestrator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	maestro "github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
)

type countingRunner struct {
	planType string
	errs     map[string]error

	mu      sync.Mutex
	run     int
	runMany int
}

func (f *countingRunner) Run(_ context.Context, plan *maestro.Plan, _ string, _ int, _ map[string]any) (any, error) {
	f.mu.Lock()
	f.run++
	f.mu.Unlock()
	if err := f.errs[plan.ID]; err != nil {
		return nil, err
	}
	if strings.HasSuffix(plan.ID, "OperStatus") {
		return "Online", nil
	}
	return "ok-" + plan.ID, nil
}

func (f *countingRunner) RunMany(_ context.Context, _ string, _ int, invs []orchestrator.Invocation) (map[string]any, error) {
	f.mu.Lock()
	f.runMany++
	f.mu.Unlock()
	out := make(map[string]any, len(invs))
	for _, inv := range invs {
		if err := f.errs[inv.Plan.ID]; err != nil {
			out[inv.Key] = err
			continue
		}
		out[inv.Key] = "ok-" + inv.Plan.ID
	}
	return out, nil
}

func (f *countingRunner) SupportsBatch() bool { return true }

func (f *countingRunner) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           f.planType,
		AllowedActions: []string{"GET"},
		PlanIO:         map[string]orchestrator.PlanTypeIO{f.planType: {DefaultAction: "GET"}},
	}
}

func (f *countingRunner) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run, f.runMany
}

type planMap map[string]*maestro.Plan

func (m planMap) GetPlan(_ context.Context, _ int, key string) (*maestro.Plan, error) {
	if p, ok := m[key]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("plan %s not found", key)
}

func (m planMap) GetPlans(_ context.Context, _ int, keys []string) (map[string]*maestro.Plan, error) {
	out := map[string]*maestro.Plan{}
	for _, k := range keys {
		if p, ok := m[k]; ok {
			out[k] = p
		}
	}
	return out, nil
}

func (m planMap) GetPlanByKey(ctx context.Context, scope int, key string) (*maestro.Plan, error) {
	return m.GetPlan(ctx, scope, key)
}

func fakePlan(id, planType string, guardOn ...string) *maestro.Plan {
	p := &maestro.Plan{ID: id, Type: planType}
	if len(guardOn) > 0 {
		var guards []maestro.Guard
		for _, dep := range guardOn {
			guards = append(guards, maestro.Guard{
				Check:  "MUST_EQUAL",
				Key:    dep,
				Params: json.RawMessage(`{"expect":["Online"],"onError":"WARN"}`),
			})
		}
		p.RunSpec = map[string]maestro.ActionSpec{"GET": {Guards: guards}}
	}
	return p
}

const snmpTimeoutText = "snmp get [.1.3.6.1.4.1.9999.1.1] on 11.63.121.202: request timeout (after 1 retries)"

func runWith(t *testing.T, stop bool, orch *orchestrator.Orchestrator, keys ...string) map[string]orchestrator.Result {
	t.Helper()
	ctx := context.Background()
	if stop {
		ctx = orchestrator.WithStopOnUnreachable(ctx)
	}
	res, err := orch.Run(ctx, "GET", 8, keys, map[string]any{"dslamIp": "11.63.121.202"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// RX guards on OperStatus with onError WARN; AdminStatus has no guard but
// shares the runner, so it goes down with the same batch.
func TestStopOnUnreachable_DepUnreachable(t *testing.T) {
	plans := planMap{
		"OperStatus":  fakePlan("OperStatus", "FAKE_SNMP"),
		"RX":          fakePlan("RX", "FAKE_SNMP", "OperStatus"),
		"AdminStatus": fakePlan("AdminStatus", "FAKE_SNMP"),
	}
	for _, tc := range []struct {
		name          string
		stop          bool
		wantRun       int
		wantRunMany   int
		wantSkipError bool
	}{
		{"option off", false, 1, 1, false},
		{"option on", true, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snmp := &countingRunner{planType: "FAKE_SNMP", errs: map[string]error{
				"OperStatus":  orchestrator.Unreachable(errors.New(snmpTimeoutText)),
				"RX":          errors.New(snmpTimeoutText),
				"AdminStatus": errors.New(snmpTimeoutText),
			}}
			orch := orchestrator.NewOrchestrator(plans, snmp, nil)
			orch.RegisterRunner(snmp)

			res := runWith(t, tc.stop, orch, "RX", "AdminStatus")

			run, runMany := snmp.calls()
			if run != tc.wantRun || runMany != tc.wantRunMany {
				t.Errorf("calls Run=%d RunMany=%d, want Run=%d RunMany=%d", run, runMany, tc.wantRun, tc.wantRunMany)
			}
			t.Logf("network calls = %d (Run=%d RunMany=%d)", run+runMany, run, runMany)

			for _, key := range []string{"RX", "AdminStatus"} {
				r := res[key]
				if r.Status != orchestrator.StatusError || !strings.Contains(r.Error, "request timeout") {
					t.Errorf("%s = %s %q, want ERROR containing \"request timeout\"", key, r.Status, r.Error)
				}
				if got := strings.Contains(r.Error, "target unreachable"); got != tc.wantSkipError {
					t.Errorf("%s error %q: contains \"target unreachable\" = %v, want %v", key, r.Error, got, tc.wantSkipError)
				}
			}
			rx := res["RX"]
			if len(rx.GuardResults) != 1 || rx.GuardResults[0].Key != "OperStatus" || rx.GuardResults[0].Status != orchestrator.StatusError {
				t.Errorf("RX guard results = %+v, want the WARN'd OperStatus guard", rx.GuardResults)
			}
			if tc.stop {
				t.Logf("RX error = %q", rx.Error)
			}
		})
	}
}

func TestStopOnUnreachable_OtherDepErrorStillDispatches(t *testing.T) {
	plans := planMap{
		"OperStatus": fakePlan("OperStatus", "FAKE_SNMP"),
		"RX":         fakePlan("RX", "FAKE_SNMP", "OperStatus"),
	}
	snmp := &countingRunner{planType: "FAKE_SNMP", errs: map[string]error{
		"OperStatus": errors.New("snmp returned no value for OperStatus (oid: .1.3.6.1.4.1.9999.1.1): agent answered noSuchInstance/noSuchObject"),
	}}
	orch := orchestrator.NewOrchestrator(plans, snmp, nil)
	orch.RegisterRunner(snmp)

	res := runWith(t, true, orch, "RX")

	if run, runMany := snmp.calls(); run != 1 || runMany != 1 {
		t.Errorf("calls Run=%d RunMany=%d, want Run=1 RunMany=1", run, runMany)
	}
	if rx := res["RX"]; rx.Status != orchestrator.StatusSuccess || rx.Value != "ok-RX" {
		t.Errorf("RX = %s %v %q, want SUCCESS ok-RX", rx.Status, rx.Value, rx.Error)
	}
}

func TestStopOnUnreachable_OnlyTheDeadRunnerIsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadType string
	}{
		{"snmp down, rest runs", "FAKE_SNMP"},
		{"rest down, snmp runs", "FAKE_REST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans := planMap{
				"OperStatus":     fakePlan("OperStatus", "FAKE_SNMP"),
				"RX":             fakePlan("RX", "FAKE_SNMP", "OperStatus"),
				"RestOperStatus": fakePlan("RestOperStatus", "FAKE_REST"),
				"RestRX":         fakePlan("RestRX", "FAKE_REST", "RestOperStatus"),
			}
			snmp := &countingRunner{planType: "FAKE_SNMP", errs: map[string]error{}}
			rest := &countingRunner{planType: "FAKE_REST", errs: map[string]error{}}
			dead, live := snmp, rest
			deadDep, deadKey, liveKey := "OperStatus", "RX", "RestRX"
			if tc.deadType == "FAKE_REST" {
				dead, live = rest, snmp
				deadDep, deadKey, liveKey = "RestOperStatus", "RestRX", "RX"
			}
			dead.errs[deadDep] = orchestrator.Unreachable(errors.New("rest: request failed: dial tcp 172.19.9.40:443: connect: connection refused"))

			orch := orchestrator.NewOrchestrator(plans, snmp, nil)
			orch.RegisterRunner(snmp)
			orch.RegisterRunner(rest)

			res := runWith(t, true, orch, "RX", "RestRX")

			if _, runMany := dead.calls(); runMany != 0 {
				t.Errorf("dead runner RunMany = %d, want 0", runMany)
			}
			if _, runMany := live.calls(); runMany != 1 {
				t.Errorf("live runner RunMany = %d, want 1", runMany)
			}
			if r := res[deadKey]; r.Status != orchestrator.StatusError || !strings.HasPrefix(r.Error, "target unreachable: skipped after "+deadDep+" failed: ") {
				t.Errorf("%s = %s %q, want the skip error", deadKey, r.Status, r.Error)
			}
			if r := res[liveKey]; r.Status != orchestrator.StatusSuccess {
				t.Errorf("%s = %s %q, want SUCCESS", liveKey, r.Status, r.Error)
			}
		})
	}
}
