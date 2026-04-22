package snmp_test

import (
	maestro "github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/parser/n8n"
)

// init wires the parser into the test-only var so runmany_test.go can build
// invocations from JSON workflows without importing parser/n8n directly
// (which would clash with import-cycle expectations from other code).
func init() {
	parseWorkflow = func(b []byte) (*maestro.Plan, error) {
		return n8n.Parse(b)
	}
}
