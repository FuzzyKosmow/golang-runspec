package n8n

import "testing"

// The n8n editor speaks JS (`null`); the Go engine speaks expr-lang (`nil`).
// Without this rewrite a plan that returns a null-ish value cannot be valid on
// both sides — `null` fails the Go compile with "unknown name null" and `nil`
// shows "invalid syntax" in the editor.
func TestRewriteNullLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		// the case this exists for
		{`={{ $json.x == null ? null : $json.x / 100 }}`,
			`={{ $json.x == nil ? nil : $json.x / 100 }}`},
		// identifier boundaries — must NOT fire
		{`={{ $json.nullable }}`, `={{ $json.nullable }}`},
		{`={{ isNull($json.x) }}`, `={{ isNull($json.x) }}`},
		{`={{ $json.nullCount + 1 }}`, `={{ $json.nullCount + 1 }}`},
		// property access — must NOT fire
		{`={{ $json.null }}`, `={{ $json.null }}`},
		// string literals — must NOT fire
		{`={{ $json.x == "null" ? 1 : 0 }}`, `={{ $json.x == "null" ? 1 : 0 }}`},
		{`={{ $json.x == 'null' }}`, `={{ $json.x == 'null' }}`},
		// every occurrence in one pass
		{`={{ a == null || b == null ? null : 1 }}`,
			`={{ a == nil || b == nil ? nil : 1 }}`},
		// interpolation mode rewrites inside each {{ }} only
		{`=prefix {{ x == null }} suffix null`, `=prefix {{ x == nil }} suffix null`},
		// non-expression strings (no "=" prefix) pass through untouched
		{`null`, `null`},
		// composes with the existing String() rule
		{`={{ String($json.x) == "" ? null : $json.x }}`,
			`={{ toString($json.x) == "" ? nil : $json.x }}`},
	}
	for _, c := range cases {
		if got := rewriteExpressionString(c.in); got != c.want {
			t.Errorf("rewriteExpressionString(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}
