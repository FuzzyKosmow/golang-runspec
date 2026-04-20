package n8n

import (
	"encoding/json"
	"strings"
	"unicode"
)

// rewriteParametersJSON walks a node's Parameters blob and rewrites every
// n8n-style expression string it finds (those starting with "=") so JS-native
// idioms like `arr.find(r => r["k"])` and `String(x).padStart(3, "0")` become
// their expr-lang equivalents. This lets plan authors use n8n-native syntax
// that previews correctly in the n8n UI while the Go engine continues to
// compile pure expr-lang.
//
// Non-JSON blobs pass through unchanged. Non-matching strings are left as-is,
// so existing plans that already use expr-lang syntax (97/108 plans today)
// are unaffected.
func rewriteParametersJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return raw
	}
	walked := walkJSONStrings(decoded)
	out, err := json.Marshal(walked)
	if err != nil {
		return raw
	}
	return out
}

func walkJSONStrings(v any) any {
	switch x := v.(type) {
	case string:
		return rewriteExpressionString(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = walkJSONStrings(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = walkJSONStrings(val)
		}
		return out
	}
	return v
}

// rewriteExpressionString accepts a raw n8n parameter string. If it is an
// expression (prefix "="), the code regions are rewritten. Static strings
// pass through.
//
// Three expression modes are recognized, matching the engine's resolveExpression:
//   - Pure wrapped code: "={{ CODE }}"           → rewrite CODE
//   - Interpolation:     "=prefix {{ X }} ..."   → rewrite each X in place
//   - Pure unwrapped:    "=CODE"                 → rewrite CODE
func rewriteExpressionString(expr string) string {
	if !strings.HasPrefix(expr, "=") {
		return expr
	}
	body := expr[1:]
	if !strings.Contains(body, "{{") {
		return "=" + rewriteJSExpressionCode(body)
	}
	var out strings.Builder
	out.WriteByte('=')
	i := 0
	for i < len(body) {
		open := strings.Index(body[i:], "{{")
		if open < 0 {
			out.WriteString(body[i:])
			break
		}
		absOpen := i + open
		rel := strings.Index(body[absOpen:], "}}")
		if rel < 0 {
			out.WriteString(body[i:])
			break
		}
		absClose := absOpen + rel
		out.WriteString(body[i:absOpen])
		out.WriteString("{{")
		out.WriteString(rewriteJSExpressionCode(body[absOpen+2 : absClose]))
		out.WriteString("}}")
		i = absClose + 2
	}
	return out.String()
}

// rewriteJSExpressionCode is the core single-expression rewriter. It handles
// the JS idioms that show up in n8n-authored plans and translates them to the
// expr-lang equivalents our Go engine accepts:
//
//	String(x)              → toString(x)
//	x.toString()           → toString(x)
//	x.padStart(n, c)       → padStart(x, n, c)
//	arr.find(p => body)    → find(arr, body with p → #)
//	arr.filter(p => body)  → filter(arr, body with p → #)
//	arr.map(p => body)     → map(arr, body with p → #)
//
// Multi-pass so chains collapse correctly: String(x).padStart(3, "0") first
// becomes toString(x).padStart(3, "0") and then padStart(toString(x), 3, "0").
// Inputs that contain no target patterns are returned verbatim.
func rewriteJSExpressionCode(code string) string {
	const maxPasses = 16
	for pass := 0; pass < maxPasses; pass++ {
		bm := buildBracketMap(code)
		if next, ok := tryRewriteStringFn(code, bm); ok {
			code = next
			continue
		}
		if next, ok := tryRewriteSimpleMethod(code, bm, "toString"); ok {
			code = next
			continue
		}
		if next, ok := tryRewriteSimpleMethod(code, bm, "padStart"); ok {
			code = next
			continue
		}
		if next, ok := tryRewriteHigherOrder(code, bm, "find"); ok {
			code = next
			continue
		}
		if next, ok := tryRewriteHigherOrder(code, bm, "filter"); ok {
			code = next
			continue
		}
		if next, ok := tryRewriteHigherOrder(code, bm, "map"); ok {
			code = next
			continue
		}
		return code
	}
	return code
}

// tryRewriteStringFn rewrites the first `String(X)` call in code to
// `toString(X)`. Word-bounded on the left so `isString` is not touched.
func tryRewriteStringFn(code string, bm bracketMap) (string, bool) {
	const target = "String"
	i := 0
	for i < len(code) {
		idx := strings.Index(code[i:], target)
		if idx < 0 {
			return code, false
		}
		abs := i + idx
		if abs > 0 && isIdentContinue(rune(code[abs-1])) {
			i = abs + len(target)
			continue
		}
		parenIdx := abs + len(target)
		for parenIdx < len(code) && isSpaceByte(code[parenIdx]) {
			parenIdx++
		}
		if parenIdx >= len(code) || code[parenIdx] != '(' {
			i = abs + len(target)
			continue
		}
		closePos, ok := bm.openToClose[parenIdx]
		if !ok {
			i = abs + len(target)
			continue
		}
		return code[:abs] + "toString" + code[parenIdx:closePos+1] + code[closePos+1:], true
	}
	return code, false
}

// tryRewriteSimpleMethod rewrites the first occurrence of
// `RECEIVER.methodName(args)` into `methodName(RECEIVER, args)`. Used for
// padStart and toString-as-method.
func tryRewriteSimpleMethod(code string, bm bracketMap, methodName string) (string, bool) {
	pattern := "." + methodName
	i := 0
	for i < len(code) {
		idx := strings.Index(code[i:], pattern)
		if idx < 0 {
			return code, false
		}
		abs := i + idx
		after := abs + len(pattern)
		if after < len(code) && isIdentContinue(rune(code[after])) {
			i = after
			continue
		}
		parenIdx := after
		for parenIdx < len(code) && isSpaceByte(code[parenIdx]) {
			parenIdx++
		}
		if parenIdx >= len(code) || code[parenIdx] != '(' {
			i = after
			continue
		}
		closePos, ok := bm.openToClose[parenIdx]
		if !ok {
			i = after
			continue
		}
		recvStart := findReceiverStart(code, abs, bm)
		if recvStart < 0 {
			i = after
			continue
		}
		receiver := code[recvStart:abs]
		args := strings.TrimSpace(code[parenIdx+1 : closePos])
		var call string
		if args == "" {
			call = methodName + "(" + receiver + ")"
		} else {
			call = methodName + "(" + receiver + ", " + args + ")"
		}
		return code[:recvStart] + call + code[closePos+1:], true
	}
	return code, false
}

// tryRewriteHigherOrder rewrites `arr.find(p => body)` (or filter/map) into
// `find(arr, body[p→#])`. Leaves the expression unchanged if the single
// argument is not a `param => body` lambda.
func tryRewriteHigherOrder(code string, bm bracketMap, methodName string) (string, bool) {
	pattern := "." + methodName
	i := 0
	for i < len(code) {
		idx := strings.Index(code[i:], pattern)
		if idx < 0 {
			return code, false
		}
		abs := i + idx
		after := abs + len(pattern)
		if after < len(code) && isIdentContinue(rune(code[after])) {
			i = after
			continue
		}
		parenIdx := after
		for parenIdx < len(code) && isSpaceByte(code[parenIdx]) {
			parenIdx++
		}
		if parenIdx >= len(code) || code[parenIdx] != '(' {
			i = after
			continue
		}
		closePos, ok := bm.openToClose[parenIdx]
		if !ok {
			i = after
			continue
		}
		recvStart := findReceiverStart(code, abs, bm)
		if recvStart < 0 {
			i = after
			continue
		}
		argsRaw := code[parenIdx+1 : closePos]
		param, body, ok2 := parseArrow(argsRaw)
		if !ok2 {
			i = after
			continue
		}
		receiver := code[recvStart:abs]
		newBody := substituteParamWithHash(body, param)
		call := methodName + "(" + receiver + ", " + strings.TrimSpace(newBody) + ")"
		return code[:recvStart] + call + code[closePos+1:], true
	}
	return code, false
}

// parseArrow parses `<ident> => <body>` (no surrounding parens). Returns the
// param name and body text. Parenthesized forms like `(r) => body` are not
// handled — if encountered, the rewriter leaves the expression alone and the
// compile step reports the error.
func parseArrow(s string) (string, string, bool) {
	s = strings.TrimLeft(s, " \t\n\r")
	j := 0
	for j < len(s) && isIdentContinue(rune(s[j])) {
		j++
	}
	if j == 0 {
		return "", "", false
	}
	param := s[:j]
	rest := strings.TrimLeft(s[j:], " \t\n\r")
	if !strings.HasPrefix(rest, "=>") {
		return "", "", false
	}
	body := strings.TrimLeft(rest[2:], " \t\n\r")
	return param, body, true
}

// substituteParamWithHash replaces whole-word occurrences of param in body
// with `#`, skipping string literals and identifiers preceded by `.` (field
// accesses like `other.param` shouldn't be rewritten).
func substituteParamWithHash(body, param string) string {
	var out strings.Builder
	i := 0
	for i < len(body) {
		c := body[i]
		switch {
		case c == '"' || c == '\'':
			end := skipStringForward(body, i)
			if end < 0 {
				out.WriteString(body[i:])
				return out.String()
			}
			out.WriteString(body[i:end])
			i = end
		case c == '`':
			end := skipBacktickForward(body, i)
			if end < 0 {
				out.WriteString(body[i:])
				return out.String()
			}
			out.WriteString(body[i:end])
			i = end
		case isIdentStart(rune(c)):
			j := i
			for j < len(body) && isIdentContinue(rune(body[j])) {
				j++
			}
			ident := body[i:j]
			var prev byte
			if i > 0 {
				prev = body[i-1]
			}
			if ident == param && prev != '.' {
				out.WriteString("#")
			} else {
				out.WriteString(ident)
			}
			i = j
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// findReceiverStart walks backward from dotPos to locate the rightmost
// expression that ends at the dot. Handles identifier chains (a.b.c), bracket
// access (a[x]), function calls (fn(a)), method-call chains (filter(a).map),
// and parenthesized groups ((x + y)). Returns -1 if no valid receiver was
// found (e.g., the dot is at the start of the expression or preceded by an
// operator).
func findReceiverStart(code string, dotPos int, bm bracketMap) int {
	i := dotPos - 1
	for i >= 0 && isSpaceByte(code[i]) {
		i--
	}
	if i < 0 {
		return -1
	}
	atomStart, i := consumeAtomBackward(code, i, bm)
	if atomStart < 0 {
		return -1
	}
	for {
		j := i
		for j >= 0 && isSpaceByte(code[j]) {
			j--
		}
		if j < 0 || code[j] != '.' {
			return atomStart
		}
		j--
		for j >= 0 && isSpaceByte(code[j]) {
			j--
		}
		if j < 0 {
			return atomStart
		}
		newStart, newI := consumeAtomBackward(code, j, bm)
		if newStart < 0 {
			return atomStart
		}
		atomStart = newStart
		i = newI
	}
}

// consumeAtomBackward consumes the rightmost receiver-atom ending at endPos.
// An atom is a run of trailers (brackets, call parens) optionally prefixed
// by an identifier, with no '.' separator inside. Examples that count as
// single atoms: `foo`, `foo()`, `foo(a,b)[c]`, `(expr)`, `$json`.
// Returns (start, newI) where newI is the index before the atom start, or
// (-1, endPos) if endPos does not begin a valid atom.
func consumeAtomBackward(code string, endPos int, bm bracketMap) (int, int) {
	i := endPos
	atomStart := -1
	for i >= 0 {
		c := code[i]
		switch {
		case c == ')' || c == ']':
			open, ok := bm.closeToOpen[i]
			if !ok {
				return atomStart, i
			}
			atomStart = open
			i = open - 1
		case isIdentContinue(rune(c)):
			for i >= 0 && isIdentContinue(rune(code[i])) {
				atomStart = i
				i--
			}
			return atomStart, i
		default:
			return atomStart, i
		}
	}
	return atomStart, i
}

type bracketMap struct {
	openToClose map[int]int
	closeToOpen map[int]int
}

// buildBracketMap does a single forward pass to pair every balanced bracket,
// skipping string contents so `"(foo)"` doesn't corrupt the stack.
func buildBracketMap(code string) bracketMap {
	bm := bracketMap{
		openToClose: map[int]int{},
		closeToOpen: map[int]int{},
	}
	type frame struct {
		ch  byte
		pos int
	}
	var stack []frame
	i := 0
	for i < len(code) {
		c := code[i]
		switch c {
		case '"', '\'':
			end := skipStringForward(code, i)
			if end < 0 {
				return bm
			}
			i = end
			continue
		case '`':
			end := skipBacktickForward(code, i)
			if end < 0 {
				return bm
			}
			i = end
			continue
		case '(', '[', '{':
			stack = append(stack, frame{c, i})
		case ')', ']', '}':
			if len(stack) == 0 {
				i++
				continue
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			bm.openToClose[top.pos] = i
			bm.closeToOpen[i] = top.pos
		}
		i++
	}
	return bm
}

func skipStringForward(code string, i int) int {
	q := code[i]
	i++
	for i < len(code) {
		if code[i] == '\\' && i+1 < len(code) {
			i += 2
			continue
		}
		if code[i] == q {
			return i + 1
		}
		i++
	}
	return -1
}

func skipBacktickForward(code string, i int) int {
	i++
	for i < len(code) {
		if code[i] == '`' {
			return i + 1
		}
		i++
	}
	return -1
}

func isIdentStart(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r)
}

func isIdentContinue(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
