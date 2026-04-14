package orchestrator

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// CloneMap creates a shallow copy of a map with extra capacity.
func CloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// CoerceNumeric attempts to turn raw result values into a numeric-friendly type.
// Handles: float, int, uint, string, json.Number, []byte (octet strings),
// and map wrappers with "value" key.
func CoerceNumeric(v any) any {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case int32:
		return float64(t)
	case uint:
		return float64(t)
	case uint64:
		return float64(t)
	case uint32:
		return float64(t)
	case []byte:
		// octet strings: try as numeric string first, then printable string, then hex
		s := strings.TrimSpace(string(t))
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		if isPrintable(s) {
			return s
		}
		return hex.EncodeToString(t)
	case string:
		ts := strings.TrimSpace(t)
		if f, err := strconv.ParseFloat(ts, 64); err == nil {
			return f
		}
		return t
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t
	case map[string]any:
		if val, ok := t["value"]; ok {
			return CoerceNumeric(val)
		}
		if len(t) == 1 {
			for _, val := range t {
				return CoerceNumeric(val)
			}
		}
		return t
	default:
		return v
	}
}

// FormatValue returns a string representation of a value for logging/comparison.
// Unwraps map wrappers and handles []byte.
func FormatValue(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if val, exists := t["value"]; exists {
			return fmt.Sprintf("%v", val)
		}
	case map[string]string:
		if val, exists := t["value"]; exists {
			return val
		}
	case []byte:
		s := string(t)
		if isPrintable(s) {
			return s
		}
		return hex.EncodeToString(t)
	}
	return fmt.Sprintf("%v", v)
}

// AppendLog is a helper to append to a log slice pointer.
func AppendLog(logs *[]string, msg string) {
	*logs = append(*logs, msg)
}

// isPrintable checks if a string contains only printable ASCII characters.
func isPrintable(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
