package snmp_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"

	maestro "github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"github.com/FuzzyKosmow/golang-runspec/runner/snmp"
)

// TestRunMany_GroupsByTarget proves the killer property: many invocations
// across multiple OLT targets collapse to ONE GetWithAlias call per unique
// (dslamIp + community) pair. This is what gives the worker per-OLT
// mega-batch parity with the old C# scanner without exposing OLT/contract
// concepts on the Runner interface.
func TestRunMany_GroupsByTarget(t *testing.T) {
	// 6 invocations across 3 targets (2 OLTs each scanned for 3 properties).
	// Expectation: 3 GetWithAlias calls — one per target, each carrying
	// 3 OIDs (one per property).
	stub := &recordingClient{
		responses: map[string]any{
			"oid-A-RX": int64(-180), "oid-A-TX": int64(20), "oid-A-OS": int64(1),
			"oid-B-RX": int64(-200), "oid-B-TX": int64(22), "oid-B-OS": int64(1),
			"oid-C-RX": int64(-150), "oid-C-TX": int64(25), "oid-C-OS": int64(1),
		},
	}

	r := snmp.NewRunner(stub)

	// Build invocations. Each has a fixed OID embedded in plan.Config.constOID
	// — easier than wiring the full plan engine for this test.
	invs := []orchestrator.Invocation{
		newConstOIDInvocation("A:RX", "oid-A-RX", "11.1.1.1", "comm1"),
		newConstOIDInvocation("A:TX", "oid-A-TX", "11.1.1.1", "comm1"),
		newConstOIDInvocation("A:OS", "oid-A-OS", "11.1.1.1", "comm1"),
		newConstOIDInvocation("B:RX", "oid-B-RX", "11.1.1.2", "comm1"),
		newConstOIDInvocation("B:TX", "oid-B-TX", "11.1.1.2", "comm1"),
		newConstOIDInvocation("B:OS", "oid-B-OS", "11.1.1.2", "comm1"),
		// Same IP as B but different community — must be its own group.
		newConstOIDInvocation("C:RX", "oid-C-RX", "11.1.1.2", "comm2"),
		newConstOIDInvocation("C:TX", "oid-C-TX", "11.1.1.2", "comm2"),
		newConstOIDInvocation("C:OS", "oid-C-OS", "11.1.1.2", "comm2"),
	}

	results, err := r.RunMany(context.Background(), snmp.ActionGET, 0, invs)
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}

	// Verify call count: 3 unique (ip, community) groups → 3 calls.
	calls := stub.snapshotCalls()
	if got, want := len(calls), 3; got != want {
		t.Fatalf("expected %d GetWithAlias calls (one per target), got %d:\n%v", want, got, calls)
	}

	// Verify each call carried exactly 3 OIDs.
	for _, c := range calls {
		if got := len(c.OIDs); got != 3 {
			t.Errorf("target %s/%s: expected 3 OIDs in bulk call, got %d", c.IP, c.Community, got)
		}
	}

	// Verify per-invocation results land under the right keys with the right values.
	cases := map[string]int64{
		"A:RX": -180, "A:TX": 20, "A:OS": 1,
		"B:RX": -200, "B:TX": 22, "B:OS": 1,
		"C:RX": -150, "C:TX": 25, "C:OS": 1,
	}
	for key, want := range cases {
		got, ok := results[key]
		if !ok {
			t.Errorf("missing result for %s", key)
			continue
		}
		if got != want {
			t.Errorf("%s: expected %v, got %v (%T)", key, want, got, got)
		}
	}
}

// TestRunMany_PerTargetFailure verifies one failing OLT doesn't tank the rest.
func TestRunMany_PerTargetFailure(t *testing.T) {
	stub := &recordingClient{
		responses: map[string]any{
			"oid-good-1": int64(42),
			"oid-good-2": int64(43),
		},
		failTargets: map[string]bool{"11.99.99.99": true},
	}

	r := snmp.NewRunner(stub)
	invs := []orchestrator.Invocation{
		newConstOIDInvocation("good:1", "oid-good-1", "11.1.1.1", "c"),
		newConstOIDInvocation("good:2", "oid-good-2", "11.1.1.1", "c"),
		newConstOIDInvocation("bad:X", "oid-bad-X", "11.99.99.99", "c"),
	}
	results, err := r.RunMany(context.Background(), snmp.ActionGET, 0, invs)
	if err != nil {
		t.Fatalf("RunMany should not return systemic error for per-target failure: %v", err)
	}

	if results["good:1"] != int64(42) {
		t.Errorf("good:1 should succeed, got %v", results["good:1"])
	}
	if results["good:2"] != int64(43) {
		t.Errorf("good:2 should succeed, got %v", results["good:2"])
	}
	bad, ok := results["bad:X"].(error)
	if !ok {
		t.Fatalf("bad:X should be an error value, got %T (%v)", results["bad:X"], results["bad:X"])
	}
	if bad == nil {
		t.Error("bad:X error should be non-nil")
	}
}

// --- helpers ---

// newConstOIDInvocation builds an Invocation whose Plan is a stub OID_GEN
// returning a constant OID via the engine. We embed the OID in a Set node so
// the plan engine produces it without needing real input fields.
func newConstOIDInvocation(key, oid, ip, community string) orchestrator.Invocation {
	workflow := `{
		"nodes": [
			{"id": "in", "name": "in", "type": "CUSTOM.dslInputDef", "parameters": {"fields": {"field": []}}},
			{"id": "out", "name": "out", "type": "CUSTOM.dslOutputDef", "parameters": {
				"planType": "OID_GEN",
				"primaryFieldName": "oid",
				"primaryValue": "` + oid + `"
			}}
		],
		"connections": {"in": {"main": [[{"node": "out", "type": "main", "index": 0}]]}}
	}`
	plan := mustParse(workflow)
	plan.Type = snmp.PlanTypeOIDGen
	return orchestrator.Invocation{
		Key:  key,
		Plan: plan,
		Inputs: map[string]any{
			"dslamIp": ip,
			"snmp":    community,
		},
	}
}

// mustParse runs the n8n parser on a stub workflow JSON and returns the Plan.
// We import parser via interface to avoid a hard dep here — defined in shared
// test helper.
func mustParse(workflow string) *maestro.Plan {
	plan, err := parseWorkflow([]byte(workflow))
	if err != nil {
		panic(err)
	}
	return plan
}

// parseWorkflow is wired up in the helper file (parsePlanHelper.go below)
// so we don't import parser/n8n in the test directly — keeps the dep
// surface local.
var parseWorkflow = func(b []byte) (*maestro.Plan, error) {
	return nil, nil // overridden in helper
}

// --- recording client ---

// recordingClient records every GetWithAlias call so tests can assert on
// per-target call count.
type recordingClient struct {
	mu          sync.Mutex
	responses   map[string]any  // alias → value
	failTargets map[string]bool // IPs that should error
	calls       []snmp.Target
	// omitUnknown switches the shape of a miss. false (default) mimics an
	// agent answering noSuchInstance: the key is present with a nil value,
	// which is what GetMultiple stores for such a PDU. true mimics the agent
	// dropping the varbind from the response entirely, which is what a
	// truncated multi-OID GetResponse looks like after GetWithAlias re-keys.
	omitUnknown bool
}

func (c *recordingClient) Get(ip, community, oid string) (any, error) {
	return nil, nil
}
func (c *recordingClient) GetMultiple(ip, community string, oids []string) (map[string]any, error) {
	return nil, nil
}
func (c *recordingClient) GetWithAlias(t snmp.Target) (map[string]any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, t)
	c.mu.Unlock()

	if c.failTargets[t.IP] {
		return nil, errSimulatedTransport
	}
	out := make(map[string]any, len(t.OIDs))
	for alias, oid := range t.OIDs {
		if v, ok := c.responses[oid]; ok {
			out[alias] = v
			continue
		}
		if !c.omitUnknown {
			out[alias] = nil
		}
	}
	return out, nil
}
func (c *recordingClient) SetInteger(ip, community, oid string, value int) error { return nil }
func (c *recordingClient) SetMultiple(ip, community string, values []snmp.SetValue) error {
	return nil
}

func (c *recordingClient) snapshotCalls() []snmp.Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]snmp.Target, len(c.calls))
	copy(out, c.calls)
	// stable order for assertion-friendly output
	sort.Slice(out, func(i, j int) bool {
		return out[i].IP+out[i].Community < out[j].IP+out[j].Community
	})
	return out
}

// errSimulatedTransport is the canned error for failTargets.
var errSimulatedTransport = simulatedErr("simulated transport failure")

type simulatedErr string

func (e simulatedErr) Error() string { return string(e) }

// jsonOK is a no-op kept in case future test cases need to inspect the
// response body shape.
var _ = json.Marshal

// TestRunMany_AbsentVarbindIsAnError is the regression test for the defect
// that made the 2026-08-22 prod shadow run undiagnosable: RunMany stored an
// unanswered OID as a nil SUCCESS, so a failed contract carried no error, no
// property name and no OID — while the same contract through Run() reported
// all three. The two absence shapes must both error, and must say which one
// happened, because they point at different causes: noSuchInstance means the
// instance is wrong, an omitted varbind means the agent truncated the reply.
func TestRunMany_AbsentVarbindIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		omit      bool
		wantCause string
	}{
		{"noSuchInstance", false, "noSuchInstance"},
		{"omitted from response", true, "varbind absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordingClient{
				responses:   map[string]any{"oid-present": int64(-190)},
				omitUnknown: tc.omit,
			}
			r := snmp.NewRunner(stub)

			results, err := r.RunMany(context.Background(), snmp.ActionGET, 0, []orchestrator.Invocation{
				newConstOIDInvocation("42:RX", "oid-present", "11.1.1.1", "comm"),
				newConstOIDInvocation("42:TX", "oid-missing", "11.1.1.1", "comm"),
			})
			if err != nil {
				t.Fatalf("RunMany: %v", err)
			}

			// The answered OID is untouched — an absent sibling in the same
			// bulk call must not cost it, or we trade a silent nil for the
			// chunk-wide collateral this whole path exists to avoid.
			if results["42:RX"] != int64(-190) {
				t.Errorf("42:RX should still succeed, got %v (%T)", results["42:RX"], results["42:RX"])
			}

			missErr, ok := results["42:TX"].(error)
			if !ok {
				t.Fatalf("42:TX should be an error value, got %T (%v)", results["42:TX"], results["42:TX"])
			}
			// The OID is the one datum the investigation lacked — assert it
			// explicitly rather than just checking for non-nil.
			if !strings.Contains(missErr.Error(), "oid-missing") {
				t.Errorf("error must name the OID, got: %v", missErr)
			}
			if !strings.Contains(missErr.Error(), tc.wantCause) {
				t.Errorf("error must distinguish the absence cause %q, got: %v", tc.wantCause, missErr)
			}
		})
	}
}

// TestRunMany_AbsentVarbindNamesTheProperty proves the error carries the plan's
// configured property rather than only the caller's invocation key, matching
// Run()'s wording so one ES query covers the bulk and single paths alike.
func TestRunMany_AbsentVarbindNamesTheProperty(t *testing.T) {
	stub := &recordingClient{responses: map[string]any{}}
	r := snmp.NewRunner(stub)

	inv := newConstOIDInvocation("42:RX", "oid-missing", "11.1.1.1", "comm")
	inv.Plan.Config = []byte(`{"property":"RX","deviceType":13}`)

	results, err := r.RunMany(context.Background(), snmp.ActionGET, 0, []orchestrator.Invocation{inv})
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}
	missErr, ok := results["42:RX"].(error)
	if !ok {
		t.Fatalf("expected an error value, got %T", results["42:RX"])
	}
	if !strings.Contains(missErr.Error(), "no value for RX") {
		t.Errorf("error should name the property RX, got: %v", missErr)
	}
}
