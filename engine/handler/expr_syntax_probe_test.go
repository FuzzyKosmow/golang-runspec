package handler

import (
	"encoding/json"
	"testing"

	maestro "github.com/FuzzyKosmow/golang-runspec"
)

// TestExprSyntaxProbe — probes which JS-like syntaxes expr-lang accepts.
// Answer the question: "if a user writes arr.find(r => predicate), does it parse?"
func TestExprSyntaxProbe(t *testing.T) {
	payload := `{
		"revisions": {
			"revision": [
				{"name": "V9.0.10P9N12", "version": "V9.0.10P9N12", "is-active": true, "is-committed": true}
			]
		}
	}`
	var obj any
	_ = json.Unmarshal([]byte(payload), &obj)

	cases := []struct {
		label      string
		expr       string
		expectWork bool
		note       string
	}{
		{
			label:      "expr-lang idiom: find(arr, #[\"key\"])",
			expr:       `find($json.snmp_value.revisions.revision, #["is-active"] && #["is-committed"]).version`,
			expectWork: true,
			note:       "canonical expr-lang — works",
		},
		{
			label:      "JS-style: arr.find(r => r[\"key\"])",
			expr:       `$json.snmp_value.revisions.revision.find(r => r["is-active"] && r["is-committed"]).version`,
			expectWork: false,
			note:       "JS arrow syntax — expr-lang may reject =>",
		},
		{
			label:      "pipe syntax: arr | find(#[...])",
			expr:       `($json.snmp_value.revisions.revision | find(#["is-active"] && #["is-committed"])).version`,
			expectWork: true,
			note:       "expr-lang pipe — alternative idiom",
		},
		{
			label:      "let-binding: let r = find(...); r.version",
			expr:       `let r = find($json.snmp_value.revisions.revision, #["is-active"]); r.version`,
			expectWork: true,
			note:       "expr-lang let-binding for readability",
		},
		{
			label:      "bracket dot-mix: $json[\"snmp_value\"].revisions.revision[0].version",
			expr:       `$json["snmp_value"].revisions.revision[0].version`,
			expectWork: true,
			note:       "direct indexed access — works for single revision",
		},
		{
			label:      "field access with hyphen via dot (should fail)",
			expr:       `$json.snmp_value.revisions.revision[0].is-active`,
			expectWork: false,
			note:       "dot-hyphen: parser treats as subtraction — fails",
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			state := maestro.NewState(map[string]any{"snmp_value": obj})
			got, err := evalSimple(tc.expr, state)
			if err != nil {
				if tc.expectWork {
					t.Errorf("EXPECTED TO WORK BUT FAILED — %s: %v", tc.note, err)
				} else {
					t.Logf("OK — expected failure (%s): %v", tc.note, err)
				}
				return
			}
			if !tc.expectWork {
				t.Errorf("EXPECTED TO FAIL BUT GOT: %v — %s", got, tc.note)
				return
			}
			t.Logf("OK — got %v (%s)", got, tc.note)
		})
	}
}
