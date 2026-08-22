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
			".1.3.6.1.4.1.9999.1.1": int64(-180), ".1.3.6.1.4.1.9999.1.2": int64(20), ".1.3.6.1.4.1.9999.1.3": int64(1),
			".1.3.6.1.4.1.9999.2.1": int64(-200), ".1.3.6.1.4.1.9999.2.2": int64(22), ".1.3.6.1.4.1.9999.2.3": int64(1),
			".1.3.6.1.4.1.9999.3.1": int64(-150), ".1.3.6.1.4.1.9999.3.2": int64(25), ".1.3.6.1.4.1.9999.3.3": int64(1),
		},
	}

	r := snmp.NewRunner(stub)

	// Build invocations. Each has a fixed OID embedded in plan.Config.constOID
	// — easier than wiring the full plan engine for this test.
	invs := []orchestrator.Invocation{
		newConstOIDInvocation("A:RX", ".1.3.6.1.4.1.9999.1.1", "11.1.1.1", "comm1"),
		newConstOIDInvocation("A:TX", ".1.3.6.1.4.1.9999.1.2", "11.1.1.1", "comm1"),
		newConstOIDInvocation("A:OS", ".1.3.6.1.4.1.9999.1.3", "11.1.1.1", "comm1"),
		newConstOIDInvocation("B:RX", ".1.3.6.1.4.1.9999.2.1", "11.1.1.2", "comm1"),
		newConstOIDInvocation("B:TX", ".1.3.6.1.4.1.9999.2.2", "11.1.1.2", "comm1"),
		newConstOIDInvocation("B:OS", ".1.3.6.1.4.1.9999.2.3", "11.1.1.2", "comm1"),
		// Same IP as B but different community — must be its own group.
		newConstOIDInvocation("C:RX", ".1.3.6.1.4.1.9999.3.1", "11.1.1.2", "comm2"),
		newConstOIDInvocation("C:TX", ".1.3.6.1.4.1.9999.3.2", "11.1.1.2", "comm2"),
		newConstOIDInvocation("C:OS", ".1.3.6.1.4.1.9999.3.3", "11.1.1.2", "comm2"),
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
			".1.3.6.1.4.1.9999.10.1": int64(42),
			".1.3.6.1.4.1.9999.10.2": int64(43),
		},
		failTargets: map[string]bool{"11.99.99.99": true},
	}

	r := snmp.NewRunner(stub)
	invs := []orchestrator.Invocation{
		newConstOIDInvocation("good:1", ".1.3.6.1.4.1.9999.10.1", "11.1.1.1", "c"),
		newConstOIDInvocation("good:2", ".1.3.6.1.4.1.9999.10.2", "11.1.1.1", "c"),
		newConstOIDInvocation("bad:X", ".1.3.6.1.4.1.9999.10.3", "11.99.99.99", "c"),
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
//
// Fixture OIDs are REAL dotted-numeric OIDs (enterprise 9999 = obviously
// synthetic), not readable placeholders like "oid-A-RX". They have to be:
// RunMany validates every generated OID before it joins a target group, so a
// placeholder is rejected in phase 1 and the test stops exercising the path it
// was written for. TestRunMany_PerTargetFailure is the sharp case — its bad
// invocation must fail at the TRANSPORT, which means its OID must be valid.

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
				responses:   map[string]any{".1.3.6.1.4.1.9999.20.1": int64(-190)},
				omitUnknown: tc.omit,
			}
			r := snmp.NewRunner(stub)

			results, err := r.RunMany(context.Background(), snmp.ActionGET, 0, []orchestrator.Invocation{
				newConstOIDInvocation("42:RX", ".1.3.6.1.4.1.9999.20.1", "11.1.1.1", "comm"),
				newConstOIDInvocation("42:TX", ".1.3.6.1.4.1.9999.20.2", "11.1.1.1", "comm"),
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
			if !strings.Contains(missErr.Error(), ".1.3.6.1.4.1.9999.20.2") {
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

	inv := newConstOIDInvocation("42:RX", ".1.3.6.1.4.1.9999.20.2", "11.1.1.1", "comm")
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

// TestValidateOID_RejectsUnresolvedTemplates covers the shapes an unresolved
// plan variable actually produces. gosnmp answers all of them with the same
// opaque "Invalid object identifier", so the point of the guard is that the
// error says WHICH component and WHY.
func TestValidateOID_RejectsUnresolvedTemplates(t *testing.T) {
	valid := []string{
		".1.3.6.1.4.1.2011.6.128.1.1.2.51.1.4.16777728.0", // real D14 shape
		"1.3.6.1.2.1.1.3.0",                      // no leading dot
		".1.3.6.1.4.1.2011.6.128.1.1.2.51.1.4.0", // onu=0 — legal, must NOT be rejected
	}
	for _, oid := range valid {
		if err := snmp.ExportValidateOID(oid); err != nil {
			t.Errorf("%q should be valid, got: %v", oid, err)
		}
	}

	invalid := []struct {
		oid  string
		want string
	}{
		{".1.3.6.1.4.1.2011.6.128.1.1.2.51.1.4.<nil>.0", "not numeric"},     // Go %v on nil
		{".1.3.6.1.4.1.2011.6.128.1.1.2.51.1.4.undefined.0", "not numeric"}, // JS unset
		{".1.3.6.1.4.1.2011.6.128.1.1.2.51.1.4..0", "is empty"},             // resolved to ""
		{"", "is empty"},
		{".1", "at least 2"},
		{".7.3.6.1", "must be 0-6"},
		{".1.40.6.1", "must be < 40"},
		{".1.3.4294967296", "32-bit"},
	}
	for _, tc := range invalid {
		err := snmp.ExportValidateOID(tc.oid)
		if err == nil {
			t.Errorf("%q should be rejected", tc.oid)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error should explain %q, got: %v", tc.oid, tc.want, err)
		}
	}
}

// TestRunMany_InvalidOIDFailsOnlyItsOwnInvocation is the regression test for
// the chunk-kill: gosnmp marshals the varbind list in one pass, so before the
// guard a single malformed OID failed every contract sharing its target.
func TestRunMany_InvalidOIDFailsOnlyItsOwnInvocation(t *testing.T) {
	stub := &recordingClient{
		responses: map[string]any{".1.3.6.1.4.1.9999.30.1": int64(-190)},
	}
	r := snmp.NewRunner(stub)

	results, err := r.RunMany(context.Background(), snmp.ActionGET, 0, []orchestrator.Invocation{
		newConstOIDInvocation("42:RX", ".1.3.6.1.4.1.2011.6.128.1.1.2.51.1.4.<nil>.0", "11.1.1.1", "comm"),
		newConstOIDInvocation("43:RX", ".1.3.6.1.4.1.9999.30.1", "11.1.1.1", "comm"),
	})
	if err != nil {
		t.Fatalf("RunMany: %v", err)
	}

	badErr, ok := results["42:RX"].(error)
	if !ok {
		t.Fatalf("42:RX should be an error value, got %T", results["42:RX"])
	}
	if !strings.Contains(badErr.Error(), "oid invalid") {
		t.Errorf("error should be attributed to the OID, got: %v", badErr)
	}

	// The whole point: the chunk-mate is untouched.
	if results["43:RX"] != int64(-190) {
		t.Errorf("43:RX must survive its neighbour's bad OID, got %v (%T)", results["43:RX"], results["43:RX"])
	}

	// And the malformed OID must never have reached the transport — if it had,
	// gosnmp would have failed the marshal for the whole target.
	for _, c := range stub.snapshotCalls() {
		for alias, oid := range c.OIDs {
			if strings.Contains(oid, "<nil>") {
				t.Errorf("invalid OID reached the client under alias %s: %s", alias, oid)
			}
		}
	}
}
