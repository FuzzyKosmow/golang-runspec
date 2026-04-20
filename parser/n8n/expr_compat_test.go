package n8n

import (
	"encoding/json"
	"testing"
)

func TestRewriteJSExpressionCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "passthrough expr-lang find()",
			in:   `find($json.items, #["is-active"]).version`,
			want: `find($json.items, #["is-active"]).version`,
		},
		{
			name: "passthrough vanilla expression",
			in:   `$json.slotID + 1`,
			want: `$json.slotID + 1`,
		},
		{
			name: "String(x) to toString(x)",
			in:   `String($json.slotID)`,
			want: `toString($json.slotID)`,
		},
		{
			name: "isString is not touched",
			in:   `isString($json.x)`,
			want: `isString($json.x)`,
		},
		{
			name: "x.toString()",
			in:   `$json.slotID.toString()`,
			want: `toString($json.slotID)`,
		},
		{
			name: "x.padStart(n, c)",
			in:   `$json.slotID.padStart(2, "0")`,
			want: `padStart($json.slotID, 2, "0")`,
		},
		{
			name: "String(x).padStart(n, c) chain",
			in:   `String($json.slotID).padStart(2, "0")`,
			want: `padStart(toString($json.slotID), 2, "0")`,
		},
		{
			name: "arr.find with bracket key",
			in:   `$json.snmp_value.revisions.revision.find(r => r["is-active"] && r["is-committed"]).version`,
			want: `find($json.snmp_value.revisions.revision, #["is-active"] && #["is-committed"]).version`,
		},
		{
			name: "arr.find with dot key",
			in:   `$json.items.find(r => r.active)`,
			want: `find($json.items, #.active)`,
		},
		{
			name: "arr.filter",
			in:   `$json.items.filter(r => r.price > 10)`,
			want: `filter($json.items, #.price > 10)`,
		},
		{
			name: "arr.map",
			in:   `$json.items.map(r => r.name)`,
			want: `map($json.items, #.name)`,
		},
		{
			name: "param rename avoids collision with field name",
			in:   `$json.items.find(rec => rec.x > other.rec)`,
			want: `find($json.items, #.x > other.rec)`,
		},
		{
			name: "string literal preserved",
			in:   `$json.items.find(r => r.name == "has.r.in.it")`,
			want: `find($json.items, #.name == "has.r.in.it")`,
		},
		{
			name: "chained filter().map() pipeline",
			in:   `$json.items.filter(r => r.ok).map(r => r.value)`,
			want: `map(filter($json.items, #.ok), #.value)`,
		},
		{
			name: "no arrow leaves .find alone",
			in:   `$json.items.find(someFn)`,
			want: `$json.items.find(someFn)`,
		},
		{
			name: "method name as prefix not rewritten",
			in:   `$json.finder($json.x)`,
			want: `$json.finder($json.x)`,
		},
		{
			name: "dot-access String is not call-rewritten",
			in:   `$json.String`,
			want: `$json.String`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteJSExpressionCode(tc.in)
			if got != tc.want {
				t.Errorf("rewriteJSExpressionCode(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewriteExpressionStringModes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "static string passthrough",
			in:   "just a label",
			want: "just a label",
		},
		{
			name: "wrapped pure code",
			in:   `={{ String($json.slotID).padStart(2, "0") }}`,
			want: `={{ padStart(toString($json.slotID), 2, "0") }}`,
		},
		{
			name: "unwrapped pure code",
			in:   `=$json.items.find(r => r.active)`,
			want: `=find($json.items, #.active)`,
		},
		{
			name: "interpolation with multiple tokens",
			in:   `={{ $json.host }}.{{ padStart($json.slot, 2, "0") }}.{{ String($json.port).padStart(2, "0") }}`,
			want: `={{ $json.host }}.{{ padStart($json.slot, 2, "0") }}.{{ padStart(toString($json.port), 2, "0") }}`,
		},
		{
			name: "interpolation with arrow lambda",
			in:   `={{ $json.items.find(r => r["is-active"]).version }}`,
			want: `={{ find($json.items, #["is-active"]).version }}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteExpressionString(tc.in)
			if got != tc.want {
				t.Errorf("rewriteExpressionString(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewriteParametersJSONWalks(t *testing.T) {
	raw := json.RawMessage(`{
		"fields": {
			"values": [
				{"name": "ont", "value": "={{ $json.host }}.{{ String($json.slot).padStart(2, \"0\") }}"},
				{"name": "firmware", "value": "={{ $json.revisions.find(r => r[\"is-active\"]).version }}"},
				{"name": "literal", "value": "static text"}
			]
		}
	}`)

	out := rewriteParametersJSON(raw)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("rewriteParametersJSON produced invalid JSON: %v", err)
	}

	values := parsed["fields"].(map[string]any)["values"].([]any)

	ontVal := values[0].(map[string]any)["value"].(string)
	wantOnt := `={{ $json.host }}.{{ padStart(toString($json.slot), 2, "0") }}`
	if ontVal != wantOnt {
		t.Errorf("ont value\n  got:  %q\n  want: %q", ontVal, wantOnt)
	}

	fwVal := values[1].(map[string]any)["value"].(string)
	wantFw := `={{ find($json.revisions, #["is-active"]).version }}`
	if fwVal != wantFw {
		t.Errorf("firmware value\n  got:  %q\n  want: %q", fwVal, wantFw)
	}

	litVal := values[2].(map[string]any)["value"].(string)
	if litVal != "static text" {
		t.Errorf("literal value should be untouched, got %q", litVal)
	}
}

func TestRewriteParametersJSONNilAndNonJSON(t *testing.T) {
	if got := rewriteParametersJSON(nil); got != nil {
		t.Errorf("nil input should return nil, got %q", got)
	}
	bogus := json.RawMessage(`not json at all`)
	if string(rewriteParametersJSON(bogus)) != string(bogus) {
		t.Errorf("non-JSON input should pass through unchanged")
	}
}
