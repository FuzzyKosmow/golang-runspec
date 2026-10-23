package rest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"github.com/FuzzyKosmow/golang-runspec/runner/rest"
)

func TestUnreachable_REST(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedHost := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer down.Close()
	downHost := strings.TrimPrefix(down.URL, "http://")

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	slowHost := strings.TrimPrefix(slow.URL, "http://")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name            string
		servers         rest.Endpoint
		ctx             context.Context
		wantUnreachable bool
		wantText        string
	}{
		{"refused on primary and secondary", rest.Endpoint{Primary: closedHost, Secondary: closedHost}, context.Background(), true, "connection refused"},
		{"refused on primary, no secondary", rest.Endpoint{Primary: closedHost}, context.Background(), true, "connection refused"},
		{"per-request timeout, no secondary", rest.Endpoint{Primary: slowHost}, context.Background(), true, "deadline exceeded"},
		{"primary 500", rest.Endpoint{Primary: downHost}, context.Background(), false, "status 500"},
		{"primary refused, secondary 500", rest.Endpoint{Primary: closedHost, Secondary: downHost}, context.Background(), false, "status 500"},
		{"primary 500, secondary refused", rest.Endpoint{Primary: downHost, Secondary: closedHost}, context.Background(), false, "connection refused"},
		{"caller ctx cancelled", rest.Endpoint{Primary: closedHost, Secondary: closedHost}, cancelled, false, "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := rest.NewRunner(http.DefaultClient, &stubPlanProvider{
				transport: &rest.TransportConfig{
					TransportKey:  "mock",
					Protocol:      "http",
					Timeout:       1,
					Servers:       map[string]rest.Endpoint{"default": tc.servers},
					ConnectionMap: map[string]string{"*": "default"},
				},
			})
			inputs := map[string]any{"parent": "*"}
			plan := mkRestPlan(t, "/oper-status", "value")

			_, runErr := runner.Run(tc.ctx, plan, rest.ActionGET, 0, inputs)
			bulk, err := runner.RunMany(tc.ctx, rest.ActionGET, 0, []orchestrator.Invocation{{Key: "OperStatus", Plan: plan, Inputs: inputs}})
			if err != nil {
				t.Fatalf("RunMany: %v", err)
			}
			bulkErr, _ := bulk["OperStatus"].(error)

			for path, err := range map[string]error{"Run": runErr, "RunMany": bulkErr} {
				if err == nil {
					t.Fatalf("%s: expected an error", path)
				}
				if got := errors.Is(err, orchestrator.ErrTargetUnreachable); got != tc.wantUnreachable {
					t.Errorf("%s: errors.Is unreachable = %v, want %v (err %v)", path, got, tc.wantUnreachable, err)
				}
				if !strings.Contains(err.Error(), tc.wantText) {
					t.Errorf("%s: err %q, want it to contain %q", path, err, tc.wantText)
				}
			}
		})
	}
}
