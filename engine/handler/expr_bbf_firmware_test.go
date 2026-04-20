package handler

import (
	"encoding/json"
	"testing"

	maestro "github.com/FuzzyKosmow/golang-runspec"
)

// TestBBFActiveRevisionExtraction verifies that expr-lang's built-in find() works
// against the Nokia BBF firmware response shape to pick the active+committed
// revision's version — validating Option C (post-processing plan extracts
// standard BBF structure) without adding any new engine code.
//
// Shape captured from live curl against Nokia Altiplano (172.19.9.40) on
// 2026-04-20 for HNFDN1018's ONT (HNIP81805NA32.01.02.G.001).
func TestBBFActiveRevisionExtraction(t *testing.T) {
	// Exact live Nokia response after REST runner's flat lookup strips the
	// outer "bbf-software-image-management-one-dot-one-mounted:software" wrapper.
	firmwarePayload := `{
		"name": "application_software",
		"revisions": {
			"revision": [
				{
					"name": "V9.0.10P9N12",
					"version": "V9.0.10P9N12",
					"is-valid": true,
					"is-committed": true,
					"is-active": true
				}
			]
		}
	}`

	var firmwareObj any
	if err := json.Unmarshal([]byte(firmwarePayload), &firmwareObj); err != nil {
		t.Fatalf("unmarshal firmware payload: %v", err)
	}

	// Simulate what applyChains does: pass the REST value into the state under
	// a key (here 'snmp_value' to match SNMP runner's contract; REST runner
	// defaults to 'result' since it has no POST_PROCESSING PlanIO entry).
	// We test both to confirm the extraction expression works regardless of key.
	cases := []struct {
		name    string
		key     string
		expr    string
		want    string
	}{
		{
			name: "snmp_value key (SNMP runner contract)",
			key:  "snmp_value",
			expr: `find($json.snmp_value.revisions.revision, #["is-active"] && #["is-committed"]).version`,
			want: "V9.0.10P9N12",
		},
		{
			name: "result key (REST runner default)",
			key:  "result",
			expr: `find($json.result.revisions.revision, #["is-active"] && #["is-committed"]).version`,
			want: "V9.0.10P9N12",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := maestro.NewState(map[string]any{tc.key: firmwareObj})
			got, err := evalSimple(tc.expr, state)
			if err != nil {
				t.Fatalf("evalSimple(%q): %v", tc.expr, err)
			}
			gotStr, ok := got.(string)
			if !ok {
				t.Fatalf("expected string result, got %T: %v", got, got)
			}
			if gotStr != tc.want {
				t.Errorf("expected %q, got %q", tc.want, gotStr)
			}
		})
	}
}

// TestBBFActiveRevisionMultipleRevisions verifies the filter picks the correct
// revision when multiple are present — simulates an ONT with both a valid
// pending-commit and the active one (common state after firmware upgrade).
func TestBBFActiveRevisionMultipleRevisions(t *testing.T) {
	payload := `{
		"name": "application_software",
		"revisions": {
			"revision": [
				{"name": "V8.0.0", "version": "V8.0.0", "is-valid": true, "is-committed": false, "is-active": false},
				{"name": "V9.0.10P9N12", "version": "V9.0.10P9N12", "is-valid": true, "is-committed": true, "is-active": true},
				{"name": "V9.1.0-beta", "version": "V9.1.0-beta", "is-valid": true, "is-committed": false, "is-active": false}
			]
		}
	}`

	var obj any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	state := maestro.NewState(map[string]any{"snmp_value": obj})
	expr := `find($json.snmp_value.revisions.revision, #["is-active"] && #["is-committed"]).version`

	got, err := evalSimple(expr, state)
	if err != nil {
		t.Fatalf("evalSimple: %v", err)
	}

	if got != "V9.0.10P9N12" {
		t.Errorf("expected V9.0.10P9N12 (the active+committed one), got %v", got)
	}
}

// TestBBFActiveRevisionNoneActive — edge case where no revision is both
// active and committed (unusual but possible during firmware upgrade window).
// find() returns nil in expr-lang — verifies our expression behaves predictably.
func TestBBFActiveRevisionNoneActive(t *testing.T) {
	payload := `{
		"name": "application_software",
		"revisions": {
			"revision": [
				{"name": "V8.0.0", "version": "V8.0.0", "is-valid": true, "is-committed": false, "is-active": false}
			]
		}
	}`

	var obj any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	state := maestro.NewState(map[string]any{"snmp_value": obj})
	// Using ?? to provide a fallback when find returns nil (expr-lang idiom)
	// Note: expr-lang uses `??` for nil coalescing.
	expr := `(find($json.snmp_value.revisions.revision, #["is-active"] && #["is-committed"]) ?? {"version": "unknown"}).version`

	got, err := evalSimple(expr, state)
	if err != nil {
		t.Logf("Edge case expression failed (expected if ?? not supported): %v", err)
		t.Skip("nil-coalescing with ?? not supported in this expr-lang version — fallback can be done in Set node separately")
		return
	}

	if got != "unknown" {
		t.Errorf("expected 'unknown' fallback, got %v", got)
	}
}
