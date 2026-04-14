package handler

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"regexp"
	"strings"
)

// HandleDataTransform performs data conversion operations (hex, base64, regex, etc.).
func HandleDataTransform(node maestro.Node, state *maestro.ExecutionState) (*maestro.ExecutionResult, error) {
	var config maestro.DataTransformConfig
	if err := json.Unmarshal(node.Parameters, &config); err != nil {
		return nil, fmt.Errorf("invalid config for DataTransform: %w", err)
	}

	outputField := config.OutputField
	if outputField == "" {
		outputField = "result"
	}

	var result any
	var err error

	switch config.Operation {
	case "STRING_MANIPULATION":
		result, err = resolveExpression(config.Template, state)
		if err != nil {
			return nil, fmt.Errorf("template evaluation failed: %w", err)
		}

	case "BASE64_TO_HEX":
		valRaw, exists := state.Context[config.InputField]
		if !exists {
			result = nil
			break
		}
		strVal := ToString(valRaw)
		bytes, err := base64.StdEncoding.DecodeString(strVal)
		if err != nil {
			result = fmt.Sprintf("ERROR: Base64 decode failed: %v", err)
		} else {
			result = hex.EncodeToString(bytes)
		}

	case "BYTES_TO_HEX":
		valRaw, exists := state.Context[config.InputField]
		if !exists {
			result = nil
			break
		}

		switch v := valRaw.(type) {
		case []byte:
			result = hex.EncodeToString(v)
		case string:
			clean := strings.ReplaceAll(v, "[", "")
			clean = strings.ReplaceAll(clean, "]", "")
			clean = strings.TrimSpace(clean)

			if clean == "" {
				result = ""
				break
			}

			parts := strings.FieldsFunc(clean, func(r rune) bool {
				return r == ' ' || r == ','
			})

			var bytes []byte
			allNumbers := true
			for _, p := range parts {
				var b int
				if _, err := fmt.Sscanf(p, "%d", &b); err == nil && b >= 0 && b <= 255 {
					bytes = append(bytes, byte(b))
				} else {
					allNumbers = false
					break
				}
			}

			if len(bytes) > 0 && allNumbers {
				result = hex.EncodeToString(bytes)
			} else {
				result = hex.EncodeToString([]byte(v))
			}

		case []interface{}:
			var bytes []byte
			ok := true
			for _, item := range v {
				if num, isNum := item.(float64); isNum {
					bytes = append(bytes, byte(num))
				} else {
					ok = false
					break
				}
			}
			if ok {
				result = hex.EncodeToString(bytes)
			} else {
				result = fmt.Sprintf("ERROR: Invalid array input for BYTES_TO_HEX: %v", v)
			}

		default:
			str := fmt.Sprintf("%v", v)
			result = hex.EncodeToString([]byte(str))
		}

	case "HEX_TO_STRING":
		valRaw, exists := state.Context[config.InputField]
		if !exists {
			result = nil
			break
		}

		strVal := ToString(valRaw)
		cleanHex := strings.Map(func(r rune) rune {
			if strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return r
			}
			return -1
		}, strVal)

		bytes, err := hex.DecodeString(cleanHex)
		if err != nil {
			result = fmt.Sprintf("ERROR: Hex decode failed: %v", err)
		} else {
			result = string(bytes)
		}

	case "REGEX_EXTRACT":
		valRaw, exists := state.Context[config.InputField]
		if !exists {
			result = nil
			break
		}
		strVal := ToString(valRaw)

		re, err := regexp.Compile(config.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}

		matches := re.FindStringSubmatch(strVal)
		if len(matches) == 0 {
			result = nil
		} else {
			if config.RegexTemplate == "$1" && len(matches) > 1 {
				result = matches[1]
			} else {
				result = matches[0]
			}
		}

	default:
		return nil, fmt.Errorf("unknown operation: %s", config.Operation)
	}

	state.Context[outputField] = result

	advancePointer(node, state)
	return &maestro.ExecutionResult{Status: maestro.StatusCompleted}, nil
}
