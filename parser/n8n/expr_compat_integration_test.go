package n8n_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
	"github.com/expr-lang/expr"
)

// TestParseRewritesJSExpressions verifies that Parse() applies the JS-compat
// rewriter to node Parameters, so plans authored with n8n-native JS idioms
// (String/padStart/find-with-arrow) end up as expr-lang in the returned Plan.
//
// Workflow shape intentionally minimal — one InputDef start, one Set node
// carrying the target expressions as values.
func TestParseRewritesJSExpressions(t *testing.T) {
	raw := []byte(`{
		"nodes": [
			{
				"id": "start-1",
				"name": "Start",
				"type": "CUSTOM.dslInputDef",
				"parameters": {"fields": {"field": [{"name": "slot", "type": "number"}]}}
			},
			{
				"id": "set-1",
				"name": "Shape",
				"type": "n8n-nodes-base.set",
				"parameters": {
					"values": {
						"string": [
							{"name": "ontIndex", "value": "={{ $json.host }}.{{ String($json.slot).padStart(2, \"0\") }}"},
							{"name": "firmware", "value": "={{ $json.revisions.find(r => r[\"is-active\"] && r[\"is-committed\"]).version }}"},
							{"name": "activeNames", "value": "={{ $json.items.filter(r => r.active).map(r => r.name) }}"}
						]
					}
				}
			}
		],
		"connections": {
			"Start": {"main": [[{"node": "Shape", "type": "main", "index": 0}]]}
		}
	}`)

	plan, err := n8n.Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	shape, ok := plan.Nodes["set-1"]
	if !ok {
		t.Fatalf("expected node set-1 in plan")
	}

	var params map[string]any
	if err := json.Unmarshal(shape.Parameters, &params); err != nil {
		t.Fatalf("node params not valid JSON after parse: %v", err)
	}
	values := params["values"].(map[string]any)["string"].([]any)

	expectedValues := map[string]string{
		"ontIndex":    `={{ $json.host }}.{{ padStart(toString($json.slot), 2, "0") }}`,
		"firmware":    `={{ find($json.revisions, #["is-active"] && #["is-committed"]).version }}`,
		"activeNames": `={{ map(filter($json.items, #.active), #.name) }}`,
	}

	for _, v := range values {
		m := v.(map[string]any)
		name := m["name"].(string)
		got := m["value"].(string)
		want, ok := expectedValues[name]
		if !ok {
			t.Errorf("unexpected value name %q in rewritten params", name)
			continue
		}
		if got != want {
			t.Errorf("rewrite mismatch for %q\n  got:  %q\n  want: %q", name, got, want)
		}
	}

	// Sanity: forbidden JS syntax must NOT appear in the final Plan
	marshaled, _ := json.Marshal(params)
	if strings.Contains(string(marshaled), "=>") {
		t.Errorf("rewritten plan still contains arrow function: %s", marshaled)
	}
	if strings.Contains(string(marshaled), ".find(") || strings.Contains(string(marshaled), ".filter(") ||
		strings.Contains(string(marshaled), ".map(") || strings.Contains(string(marshaled), ".padStart(") {
		t.Errorf("rewritten plan still contains JS method calls: %s", marshaled)
	}
}

// TestParseRewriteEvaluatesAgainstLiveBBF parses a plan whose Set node uses
// n8n-native JS to extract the active+committed firmware revision, pulls the
// rewritten expression out of the Plan, and runs it through expr-lang against
// the live Nokia BBF payload captured on 2026-04-20. This proves the parser
// output is not just syntactically valid but semantically correct for the
// target use case (Firmware POST_PROC).
func TestParseRewriteEvaluatesAgainstLiveBBF(t *testing.T) {
	raw := []byte(`{
		"nodes": [
			{
				"id": "start",
				"name": "Start",
				"type": "CUSTOM.dslInputDef",
				"parameters": {"fields": {"field": []}}
			},
			{
				"id": "extract",
				"name": "Extract",
				"type": "n8n-nodes-base.set",
				"parameters": {
					"values": {
						"string": [
							{"name": "version", "value": "={{ $json.snmp_value.revisions.revision.find(r => r[\"is-active\"] && r[\"is-committed\"]).version }}"}
						]
					}
				}
			}
		],
		"connections": {
			"Start": {"main": [[{"node": "Extract", "type": "main", "index": 0}]]}
		}
	}`)

	plan, err := n8n.Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	var params map[string]any
	if err := json.Unmarshal(plan.Nodes["extract"].Parameters, &params); err != nil {
		t.Fatalf("params decode: %v", err)
	}
	valueStr := params["values"].(map[string]any)["string"].([]any)[0].(map[string]any)["value"].(string)

	const wantPrefix = "={{ "
	const wantSuffix = " }}"
	if !strings.HasPrefix(valueStr, wantPrefix) || !strings.HasSuffix(valueStr, wantSuffix) {
		t.Fatalf("unexpected rewritten value wrapping: %q", valueStr)
	}
	code := strings.TrimSuffix(strings.TrimPrefix(valueStr, wantPrefix), wantSuffix)

	livePayload := `{
		"name": "application_software",
		"revisions": {
			"revision": [
				{"name": "V8.0.0", "version": "V8.0.0", "is-valid": true, "is-committed": false, "is-active": false},
				{"name": "V9.0.10P9N12", "version": "V9.0.10P9N12", "is-valid": true, "is-committed": true, "is-active": true}
			]
		}
	}`
	var firmware any
	if err := json.Unmarshal([]byte(livePayload), &firmware); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}

	env := map[string]any{
		"$json": map[string]any{"snmp_value": firmware},
	}
	program, err := expr.Compile(code, expr.Env(env))
	if err != nil {
		t.Fatalf("rewritten expression failed to compile: %v\nexpr: %s", err, code)
	}
	got, err := expr.Run(program, env)
	if err != nil {
		t.Fatalf("rewritten expression failed at runtime: %v", err)
	}
	if got != "V9.0.10P9N12" {
		t.Errorf("expected V9.0.10P9N12, got %v", got)
	}
}
