package handler

import (
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"strconv"
	"strings"

	"github.com/expr-lang/expr"
)

// resolveExpression evaluates strings like "={{ $json.slotID }}" against the context.
func resolveExpression(expression string, state *maestro.ExecutionState) (any, error) {
	if !strings.HasPrefix(expression, "=") {
		return expression, nil // Static string
	}

	code := expression[1:]

	codeTrimmed := strings.TrimSpace(code)
	isWrapped := strings.HasPrefix(codeTrimmed, "{{") && strings.HasSuffix(codeTrimmed, "}}") && strings.Count(codeTrimmed, "{{") == 1

	if isWrapped {
		// Pure Code Mode (e.g. Logic blocks)
		code = strings.TrimSuffix(strings.TrimPrefix(codeTrimmed, "{{"), "}}")
	} else if strings.Contains(code, "{{") {
		// Interpolation Mode (e.g. OID string)
		finalStr := code
		for {
			start := strings.Index(finalStr, "{{")
			if start == -1 {
				break
			}
			end := strings.Index(finalStr[start:], "}}")
			if end == -1 {
				break
			}
			end += start

			tokenCode := finalStr[start+2 : end]
			tokenCode = strings.TrimSpace(tokenCode)

			val, err := evalSimple(tokenCode, state)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate token '%s': %w", tokenCode, err)
			}

			finalStr = finalStr[:start] + ToString(val) + finalStr[end+2:]
		}
		return finalStr, nil
	}

	return evalSimple(code, state)
}

func evalSimple(code string, state *maestro.ExecutionState) (any, error) {
	fnNodeLookup := func(params ...any) (any, error) {
		if len(params) == 0 {
			return nil, fmt.Errorf("node name required")
		}
		nodeName, ok := params[0].(string)
		if !ok {
			return nil, fmt.Errorf("node name must be a string")
		}
		if state.NodeOutputs != nil {
			if output, ok := state.NodeOutputs[nodeName]; ok {
				return map[string]any{
					"item": map[string]any{
						"json": output,
					},
				}, nil
			}
		}
		return map[string]any{
			"item": map[string]any{
				"json": map[string]any{},
			},
		}, nil
	}

	fnHexToString := func(params ...any) (any, error) {
		if len(params) == 0 {
			return "", nil
		}
		hexStr, ok := params[0].(string)
		if !ok {
			return params[0], nil
		}
		cleanHex := strings.Map(func(r rune) rune {
			if strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return r
			}
			return -1
		}, hexStr)

		bytes := make([]byte, 0, len(cleanHex)/2)
		for i := 0; i < len(cleanHex); i += 2 {
			if i+1 >= len(cleanHex) {
				break
			}
			val, err := strconv.ParseUint(cleanHex[i:i+2], 16, 8)
			if err == nil {
				bytes = append(bytes, byte(val))
			}
		}
		return string(bytes), nil
	}

	env := map[string]any{
		"$json":       state.Context,
		"json":        state.Context,
		"$":           fnNodeLookup,
		"hexToString": fnHexToString,
	}
	for k, v := range state.Context {
		env[k] = v
	}

	program, err := expr.Compile(code, expr.Env(env))
	if err != nil {
		return nil, err
	}

	output, err := expr.Run(program, env)
	if err != nil {
		return nil, err
	}
	return output, nil
}

// ResolveString is a wrapper that ensures the output is a string.
func ResolveString(expression string, state *maestro.ExecutionState) (string, error) {
	val, err := resolveExpression(expression, state)
	if err != nil {
		return "", err
	}
	return ToString(val), nil
}

// TryParseNumber attempts to convert a string value to a number.
func TryParseNumber(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	s = strings.TrimSpace(s)

	if strings.HasPrefix(s, ".") {
		return s
	}

	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return v
}

// ToString formats values, avoiding scientific notation for whole numbers.
func ToString(v any) string {
	switch val := v.(type) {
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case float32:
		if val == float32(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(float64(val), 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case int32:
		return strconv.FormatInt(int64(val), 10)
	case string:
		return val
	default:
		return fmt.Sprintf("%v", v)
	}
}

// advancePointer moves the instruction pointer to the next node's main output.
func advancePointer(node maestro.Node, state *maestro.ExecutionState) {
	if nexts, ok := node.Outputs["main"]; ok && len(nexts) > 0 {
		state.InstructionPointer = nexts[0].NodeID
	} else {
		state.InstructionPointer = ""
	}
}
