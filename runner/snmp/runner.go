package snmp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Actions this runner supports. Services use these constants
// instead of magic strings when calling the orchestrator.
//
// Usage:
//
//	orchestrator.Run(ctx, snmp.ActionGET, scope, keys, inputs)
const (
	ActionGET  = "GET"  // SNMP GET — read a single OID value
	ActionSET  = "SET"  // SNMP SET — write a value to an OID
	ActionWALK = "WALK" // SNMP WALK — enumerate an OID subtree
)

// Plan types claimed by this runner.
const (
	// PlanTypeOIDGen is the primary SNMP plan type (graph → OID → SNMP fetch).
	PlanTypeOIDGen = "OID_GEN"

	// PlanTypePOSTProcessing is the SNMP-specific post-processing plan type.
	// Chain input from the parent OID_GEN plan is injected under "snmp_value".
	// Renamed from "POST_PROCESSING" in v0.1.4 so each runner owns its own
	// post-processing type; legacy "POST_PROCESSING" plans remain functional
	// but fall through to the orchestrator default "result" key.
	PlanTypePOSTProcessing = "POST_PROC_SNMP"
)

// planConfig is decoded from plan.Config to extract SNMP-specific fields.
type planConfig struct {
	DeviceType  int    `json:"deviceType"`
	DeviceTypes []int  `json:"deviceTypes,omitempty"`
	Property    string `json:"property"`
	PlanKey     string `json:"planKey"`
}

func decodePlanConfig(plan *maestro.Plan) planConfig {
	var cfg planConfig
	if len(plan.Config) > 0 {
		json.Unmarshal(plan.Config, &cfg)
	}
	return cfg
}

// Runner implements orchestrator.Runner for SNMP-based plan execution.
// For OID_GEN plans: execute plan → extract OID → SNMP fetch → raw value.
// For other plans (LOGIC, POST_PROC_SNMP): execute directly via engine.
//
// Shared by any service that needs SNMP: scan API, RX updater, bulk scanner.
type Runner struct {
	eng    *engine.Engine
	client Client
}

// NewRunner creates an SNMP runner with the given SNMP client.
func NewRunner(client Client) *Runner {
	eng := engine.New()
	eng.RegisterStandardHandlers()
	return &Runner{eng: eng, client: client}
}

func (r *Runner) Run(ctx context.Context, plan *maestro.Plan, action string, scope int, inputs map[string]any) (any, error) {
	span := trace.SpanFromContext(ctx)

	switch plan.Type {
	case PlanTypeOIDGen:
		state := maestro.NewState(inputs)
		result, err := r.eng.Execute(plan, state)
		if err != nil {
			return nil, fmt.Errorf("oid gen failed: %w", err)
		}
		if result.Status != maestro.StatusCompleted {
			return nil, fmt.Errorf("oid gen did not complete (status: %s)", result.Status)
		}

		oidStr, err := extractOID(result.PrimitiveValue)
		if err != nil {
			return nil, fmt.Errorf("oid extract failed: %w", err)
		}
		if err := validateOID(oidStr); err != nil {
			return nil, fmt.Errorf("oid invalid: %w", err)
		}

		if r.client == nil {
			return nil, fmt.Errorf("snmp client not configured")
		}
		target, err := buildTarget(inputs, oidStr)
		if err != nil {
			return nil, err
		}

		// Action determines SNMP operation
		switch action {
		case "GET":
			vals, err := r.client.GetWithAlias(target)
			if err != nil {
				return nil, fmt.Errorf("snmp get failed: %w", err)
			}

			rawVal := extractValue(vals)
			if rawVal == nil {
				cfg := decodePlanConfig(plan)
				return nil, fmt.Errorf("snmp returned no value for %s (oid: %s)", cfg.Property, oidStr)
			}

			span.AddEvent(fmt.Sprintf("SNMP %s: %s → %v (%T)", action, oidStr, rawVal, rawVal))
			return rawVal, nil

		case "WALK":
			// Future: SNMP WALK implementation
			return nil, fmt.Errorf("WALK not yet implemented")

		case "SET":
			// Future: SNMP SET implementation
			return nil, fmt.Errorf("SET not yet implemented")

		default:
			return nil, fmt.Errorf("unsupported action %q for OID_GEN plan", action)
		}

	default:
		return r.execPlan(plan, inputs)
	}
}

// RunMany executes many (plan, inputs) pairs, grouping OID_GEN invocations by
// SNMP target (dslamIp + community) so each unique target costs ONE bulk
// GetWithAlias regardless of how many ONUs/properties feed into it. This is
// what gives the worker per-OLT mega-batch throughput without exposing
// "OLT" / "contract" concepts to the runner — grouping is a side effect of
// the standard Inputs keys (dslamIp, snmp).
//
// Per-target HTTP/SNMP failures and per-invocation OID-gen failures land as
// `error` values in the results map (partial-success); only systemic problems
// (missing client, etc.) are returned as the second value.
func (r *Runner) RunMany(ctx context.Context, action string, scope int, invocations []orchestrator.Invocation) (map[string]any, error) {
	span := trace.SpanFromContext(ctx)
	results := make(map[string]any, len(invocations))

	type oidEntry struct {
		invKey string
		oid    string
		// prop is the plan's configured property name ("RX", "TX", …). Carried
		// from phase 1 purely so an absent varbind can be reported with the
		// same wording Run() uses — the single path names the property, and a
		// bulk failure that only says "invocation 12345:RX" is one indirection
		// away from being actionable.
		prop string
	}
	type targetKey struct {
		ip        string
		community string
	}

	groups := make(map[targetKey][]oidEntry)
	var nonOID []orchestrator.Invocation

	// Phase 1: per-invocation OID_GEN execution. Each OID_GEN plan runs
	// against its own inputs (so per-ONU slotID/portID/onuID parameters
	// produce per-ONU OIDs even when bundled into one bulk).
	for _, inv := range invocations {
		if inv.Plan == nil {
			results[inv.Key] = fmt.Errorf("nil plan for invocation %s", inv.Key)
			continue
		}
		if inv.Plan.Type != PlanTypeOIDGen {
			nonOID = append(nonOID, inv)
			continue
		}

		state := maestro.NewState(inv.Inputs)
		result, err := r.eng.Execute(inv.Plan, state)
		if err != nil {
			results[inv.Key] = fmt.Errorf("oid gen: %w", err)
			continue
		}
		if result.Status != maestro.StatusCompleted {
			results[inv.Key] = fmt.Errorf("oid gen did not complete (status: %s)", result.Status)
			continue
		}
		oidStr, err := extractOID(result.PrimitiveValue)
		if err != nil {
			results[inv.Key] = fmt.Errorf("oid extract: %w", err)
			continue
		}
		// Reject a malformed OID HERE, before it joins a target group. gosnmp
		// marshals the whole varbind list in one pass, so a single unmarshalable
		// OID fails the entire bulk GET client-side — the PDU never leaves the
		// pod — and RunMany's per-target error path then attributes that failure
		// to every invocation in the group. One bad contract took down its ~15
		// chunk-mates on the 2026-08-22 prod run.
		//
		// The usual source is a plan template variable that did not resolve:
		// extractOID formats with %v, so an unset value arrives as "<nil>" (or
		// "undefined" from a JS expression) — a non-numeric component. Failing
		// just this invocation keeps the blast radius at the one contract whose
		// topology is actually wrong.
		if err := validateOID(oidStr); err != nil {
			results[inv.Key] = fmt.Errorf("oid invalid: %w", err)
			continue
		}

		ip, community, err := extractTransport(inv.Inputs)
		if err != nil {
			results[inv.Key] = err
			continue
		}
		tk := targetKey{ip: ip, community: community}
		groups[tk] = append(groups[tk], oidEntry{
			invKey: inv.Key,
			oid:    oidStr,
			prop:   decodePlanConfig(inv.Plan).Property,
		})
	}

	// Phase 2: per-target bulk SNMP fetch.
	if len(groups) > 0 && r.client == nil {
		return nil, fmt.Errorf("snmp client missing for batch fetch (%d targets queued)", len(groups))
	}
	for tk, entries := range groups {
		oidsByAlias := make(map[string]string, len(entries))
		for _, e := range entries {
			oidsByAlias[e.invKey] = e.oid
		}
		target := Target{IP: tk.ip, Community: tk.community, OIDs: oidsByAlias}

		span.AddEvent(fmt.Sprintf("SNMP RunMany %s: %d OIDs → %s", action, len(entries), tk.ip))

		snmpResults, err := r.client.GetWithAlias(target)
		if err != nil {
			// Per-target failure: every invocation pinned to this target
			// gets the same error so callers can attribute it.
			for _, e := range entries {
				results[e.invKey] = fmt.Errorf("snmp bulk fetch %s: %w", tk.ip, err)
			}
			continue
		}

		for _, e := range entries {
			rawVal, answered := lookupBulkValue(snmpResults, e.invKey, e.oid)
			if rawVal == nil {
				// An absent varbind is an ERROR, exactly as it is in Run().
				// Storing a bare nil here would record StatusSuccess with a
				// nil value (RunInvocations only treats an `error` value as a
				// failure), making "the agent did not answer this OID" and
				// "the reading is genuinely absent" indistinguishable — to the
				// caller, to the chains that then run on nil, and ultimately to
				// the DB column. Measured on prod 2026-08-22: 1,508 D13
				// contracts failed with an empty failedProperties list and no
				// error anywhere, while instant-scan (which goes through Run())
				// read the same contracts fine.
				results[e.invKey] = absentVarbindError(e.prop, e.invKey, e.oid, answered)
				span.AddEvent(fmt.Sprintf("  %s: oid=%s ABSENT (answered=%v)", e.invKey, e.oid, answered))
				continue
			}
			results[e.invKey] = rawVal
			span.AddEvent(fmt.Sprintf("  %s: oid=%s raw=%v (%T)", e.invKey, e.oid, rawVal, rawVal))
		}
	}

	// Phase 3: non-OID_GEN plans (POST_PROC etc.) execute via engine,
	// one at a time with their own inputs.
	for _, inv := range nonOID {
		val, err := r.execPlan(inv.Plan, inv.Inputs)
		if err != nil {
			results[inv.Key] = fmt.Errorf("exec: %w", err)
			continue
		}
		results[inv.Key] = val
	}

	return results, nil
}

// extractTransport pulls the SNMP target identity (IP + community) from a
// generic inputs map. Accepts both dslamIp/DslamIP and snmp/SNMP variants.
// The returned (ip, community) pair is what RunMany groups invocations by.
func extractTransport(inputs map[string]any) (string, string, error) {
	ip, _ := inputs["dslamIp"].(string)
	if ip == "" {
		ip, _ = inputs["DslamIP"].(string)
	}
	community, _ := inputs["snmp"].(string)
	if community == "" {
		community, _ = inputs["SNMP"].(string)
	}
	if ip == "" || community == "" {
		return "", "", fmt.Errorf("missing dslamIp or snmp community in inputs")
	}
	return ip, community, nil
}

func (r *Runner) SupportsBatch() bool {
	return r.client != nil
}

func (r *Runner) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           "snmp",
		AllowedActions: []string{"GET", "SET", "WALK"},
		Inputs: []orchestrator.ContractInput{
			{Key: "dslamIp", Type: "string", Required: true, NonEmpty: true, Description: "DSLAM/OLT IP address for SNMP target"},
			{Key: "snmp", Type: "string", Required: true, NonEmpty: true, Description: "SNMP community string"},
			{Key: "slotID", Type: "number", Required: false, Description: "Slot ID (0 if device has no slots, null-safe)"},
			{Key: "portID", Type: "number", Required: false, Description: "Port ID (null-safe)"},
			{Key: "onuID", Type: "number", Required: false, Description: "ONU ID (null-safe)"},
		},
		PlanIO: map[string]orchestrator.PlanTypeIO{
			PlanTypeOIDGen: {
				DefaultAction:   "GET",
				ContextInputs:   nil,             // uses service inputs directly (slotID, portID, onuID)
				RequiredOutputs: []string{"oid"}, // runner reads result["oid"] for SNMP fetch
			},
			PlanTypePOSTProcessing: {
				DefaultAction: "EXECUTE",
				ContextInputs: []orchestrator.ContractInput{
					{Key: "snmp_value", Type: "any", Required: true, Description: "Raw SNMP value, injected by orchestrator from runner result"},
				},
				RequiredOutputs: []string{"value"}, // final mapped result read by frontend
			},
		},
	}
}

func (r *Runner) execPlan(plan *maestro.Plan, inputs map[string]any) (any, error) {
	state := maestro.NewState(inputs)
	for i := 0; i < 100; i++ {
		result, err := r.eng.Execute(plan, state)
		if err != nil {
			return nil, err
		}
		switch result.Status {
		case maestro.StatusCompleted:
			return result.PrimitiveValue, nil
		case maestro.StatusPaused:
			return nil, fmt.Errorf("plan paused (not supported in runner)")
		case maestro.StatusFailed:
			return nil, fmt.Errorf("plan failed")
		}
	}
	return nil, fmt.Errorf("execution limit reached")
}

// --- Helpers ---

func extractOID(value any) (string, error) {
	if m, ok := value.(map[string]any); ok {
		if oid, exists := m["oid"]; exists {
			return fmt.Sprintf("%v", oid), nil
		}
	}
	if str, ok := value.(string); ok {
		return str, nil
	}
	return "", fmt.Errorf("cannot extract OID from result type %T", value)
}

// validateOID reports whether gosnmp can marshal this OID, and if not, WHICH
// component is at fault. It deliberately duplicates gosnmp's rules
// (marshalObjectIdentifier in helper.go) rather than probing with a trial
// marshal, because gosnmp returns one opaque string — "Invalid object
// identifier" — for every rejection, with no offset, no component and no OID.
// That message is what a failing contract used to carry all the way to ES.
//
// Stricter than gosnmp in exactly one place: gosnmp SKIPS empty components, so
// "…1.4..7" silently marshals to a valid OID pointing at the WRONG instance.
// An empty component means a template variable resolved to "", which is the
// same authoring bug as "<nil>" and should not be answered with a plausible
// reading from some other ONU.
func validateOID(oid string) error {
	if oid == "" {
		return fmt.Errorf("OID is empty")
	}
	parts := strings.Split(strings.TrimPrefix(oid, "."), ".")
	for i, part := range parts {
		if part == "" {
			return fmt.Errorf("component %d of %q is empty — a template variable resolved to \"\"", i+1, oid)
		}
		val, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return fmt.Errorf("component %d of %q is not numeric (%q) — a template variable did not resolve", i+1, oid, part)
		}
		switch i {
		case 0:
			if val > 6 {
				return fmt.Errorf("component 1 of %q must be 0-6, got %d", oid, val)
			}
		case 1:
			if val >= 40 {
				return fmt.Errorf("component 2 of %q must be < 40, got %d", oid, val)
			}
		default:
			if val > maxSubIdentifier {
				return fmt.Errorf("component %d of %q exceeds the 32-bit sub-identifier limit: %d", i+1, oid, val)
			}
		}
	}
	if len(parts) < 2 {
		return fmt.Errorf("OID %q has %d component(s); at least 2 are required", oid, len(parts))
	}
	if len(parts) > 128 {
		return fmt.Errorf("OID %q has %d components; the limit is 128", oid, len(parts))
	}
	return nil
}

// maxSubIdentifier mirrors gosnmp.MaxObjectSubIdentifierValue (2^32-1). Held
// locally so the validator does not drag a gosnmp import into this file — the
// Client interface is the only place the library belongs.
const maxSubIdentifier = 4294967295

// lookupBulkValue resolves one entry's value from a bulk result map. It
// accepts either the invocation key (what the standard GetWithAlias returns,
// since it re-keys by alias) or the raw OID, because a Client implementation
// is free to key by OID instead.
//
// The bool answers a DIFFERENT question from the value: it reports whether the
// agent returned a varbind for this OID AT ALL. A present-but-nil entry is the
// agent saying noSuchInstance/noSuchObject — it knows the OID and denies the
// instance. An absent entry means the OID never came back, which on a
// multi-varbind GET points at the AGENT truncating the response rather than at
// the instance being wrong. Those two lead to opposite investigations, so the
// error message keeps them apart.
func lookupBulkValue(results map[string]any, invKey, oid string) (any, bool) {
	if v, ok := results[invKey]; ok {
		if v != nil {
			return v, true
		}
		return nil, true
	}
	if v, ok := results[oid]; ok {
		if v != nil {
			return v, true
		}
		return nil, true
	}
	return nil, false
}

// absentVarbindError builds the error stored for an OID the bulk GET did not
// answer. Wording mirrors Run()'s single-invocation error ("snmp returned no
// value for %s (oid: %s)") so the same ES query matches both paths, with the
// cause appended.
//
// prop falls back to the invocation key: the key is caller-chosen (the worker
// uses "<contractID>:<property>"), so it always carries enough to identify the
// row even when a plan omits config.property.
func absentVarbindError(prop, invKey, oid string, answered bool) error {
	if prop == "" {
		prop = invKey
	}
	if answered {
		return fmt.Errorf("snmp returned no value for %s (oid: %s): agent answered noSuchInstance/noSuchObject", prop, oid)
	}
	return fmt.Errorf("snmp returned no value for %s (oid: %s): varbind absent from the bulk response", prop, oid)
}

func extractValue(vals map[string]any) any {
	if v, ok := vals["value"]; ok {
		return v
	}
	if len(vals) == 1 {
		for _, v := range vals {
			return v
		}
	}
	return nil
}

func buildTarget(inputs map[string]any, oid string) (Target, error) {
	ip, _ := inputs["dslamIp"].(string)
	if ip == "" {
		ip, _ = inputs["DslamIP"].(string)
	}
	community, _ := inputs["snmp"].(string)
	if community == "" {
		community, _ = inputs["SNMP"].(string)
	}
	if ip == "" || community == "" {
		return Target{}, fmt.Errorf("missing dslamIp or snmp community in inputs")
	}
	return Target{IP: ip, Community: community, OIDs: map[string]string{"value": oid}}, nil
}

// BuildBulkTarget constructs a target for fetching multiple OIDs at once.
func BuildBulkTarget(inputs map[string]any, oidBatch map[string]string) (Target, error) {
	ip, _ := inputs["dslamIp"].(string)
	if ip == "" {
		ip, _ = inputs["DslamIP"].(string)
	}
	community, _ := inputs["snmp"].(string)
	if community == "" {
		community, _ = inputs["SNMP"].(string)
	}
	if ip == "" || community == "" {
		return Target{}, fmt.Errorf("missing dslamIp or snmp community in inputs")
	}
	oids := make(map[string]string, len(oidBatch))
	for alias, oid := range oidBatch {
		oids[alias] = oid
	}
	return Target{IP: ip, Community: community, OIDs: oids}, nil
}
