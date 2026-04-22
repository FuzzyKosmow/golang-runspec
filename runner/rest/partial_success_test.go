package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	maestro "github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
	"github.com/FuzzyKosmow/golang-runspec/runner/rest"
)

// TestRunBatchPartialSuccess verifies that per-URL and per-key failures
// surface as error values in the results map — the batch as a whole still
// returns nil so sibling keys can succeed.
//
// Scenarios exercised in one batch:
//   good: 200 + field present           → typed value
//   miss: 200 + field absent            → error (missing-field with available keys)
//   down: 500 response                  → error (HTTP failure)
func TestRunBatchPartialSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/good"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": "V9.0.10P9N12"})
		case strings.HasSuffix(r.URL.Path, "/miss"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"other-field": "wrong"})
		case strings.HasSuffix(r.URL.Path, "/down"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	runner := rest.NewRunner(ts.Client(), &stubPlanProvider{
		transport: &rest.TransportConfig{
			TransportKey:  "mock",
			Protocol:      "http",
			Servers:       map[string]rest.Endpoint{"default": {Primary: strings.TrimPrefix(ts.URL, "http://")}},
			ConnectionMap: map[string]string{"*": "default"},
		},
	})

	sharedInputs := map[string]any{"parent": "*"}
	invocations := []orchestrator.Invocation{
		{Key: "good", Plan: mkRestPlan(t, "/good", "value"), Inputs: sharedInputs},
		{Key: "miss", Plan: mkRestPlan(t, "/miss", "value"), Inputs: sharedInputs},
		{Key: "down", Plan: mkRestPlan(t, "/down", "value"), Inputs: sharedInputs},
	}

	ctx := context.Background()
	results, err := runner.RunMany(ctx, rest.ActionGET, 0, invocations)
	if err != nil {
		t.Fatalf("RunMany should return nil error under partial failure, got: %v", err)
	}

	if got, want := results["good"], "V9.0.10P9N12"; got != want {
		t.Errorf("good: expected %q, got %v", want, got)
	}

	missVal, ok := results["miss"]
	if !ok {
		t.Fatalf("miss key missing from results")
	}
	missErr, ok := missVal.(error)
	if !ok {
		t.Fatalf("miss: expected error value, got %T (%v)", missVal, missVal)
	}
	if !strings.Contains(missErr.Error(), "response field") || !strings.Contains(missErr.Error(), "other-field") {
		t.Errorf("miss error should name the missing field and list available keys, got: %v", missErr)
	}

	downVal, ok := results["down"]
	if !ok {
		t.Fatalf("down key missing from results")
	}
	downErr, ok := downVal.(error)
	if !ok {
		t.Fatalf("down: expected error value, got %T (%v)", downVal, downVal)
	}
	if !strings.Contains(downErr.Error(), "500") {
		t.Errorf("down error should reference status 500, got: %v", downErr)
	}
}

// mkRestPlan builds a minimal REST_PROPERTY plan whose graph outputs a fixed
// url + responseField. Routes through the n8n parser so node types/connections
// match what the engine expects.
func mkRestPlan(t *testing.T, urlPath, responseField string) *maestro.Plan {
	t.Helper()
	workflow := `{
		"nodes": [
			{"id": "in", "name": "in", "type": "CUSTOM.dslInputDef", "parameters": {"fields": {"field": []}}},
			{"id": "out", "name": "out", "type": "CUSTOM.dslOutputDef", "parameters": {
				"planType": "REST_PROPERTY",
				"primaryFieldNameRest": "url",
				"primaryValue": "` + urlPath + `",
				"outputs": {"output": [{"name": "responseField", "value": "` + responseField + `"}]}
			}}
		],
		"connections": {
			"in": {"main": [[{"node": "out", "type": "main", "index": 0}]]}
		}
	}`
	plan, err := n8n.Parse([]byte(workflow))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	plan.Type = rest.PlanTypeRESTProperty
	cfg, _ := json.Marshal(map[string]any{"transport": "mock"})
	plan.Config = cfg
	return plan
}

// stubPlanProvider satisfies orchestrator.PlanProvider for loadTransport —
// returns a single preloaded transport config regardless of lookup key.
type stubPlanProvider struct {
	transport *rest.TransportConfig
}

func (s *stubPlanProvider) GetPlanByID(context.Context, string) (*maestro.Plan, error) {
	return nil, nil
}
func (s *stubPlanProvider) GetPlans(context.Context, int, []string) (map[string]*maestro.Plan, error) {
	return nil, nil
}
func (s *stubPlanProvider) GetPlan(context.Context, int, string) (*maestro.Plan, error) {
	return nil, nil
}
func (s *stubPlanProvider) GetPlanByKey(_ context.Context, _ int, _ string) (*maestro.Plan, error) {
	cfg, _ := json.Marshal(s.transport)
	return &maestro.Plan{Type: rest.PlanTypeTransportConfig, Config: cfg}, nil
}
