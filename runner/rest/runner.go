package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"github.com/FuzzyKosmow/golang-runspec/orchestrator"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Actions supported by the REST runner.
const (
	ActionGET = "GET" // REST GET — read a value from a RESTCONF endpoint
)

// Plan types handled by this runner.
const (
	// PlanTypeRESTProperty is a direct property fetch:
	// plan graph builds a URL path + response field name, runner resolves the server
	// from transport config, makes the HTTP call, extracts the field, applies transform.
	PlanTypeRESTProperty = "REST_PROPERTY"

	// PlanTypeTransportConfig is NOT executed — it's a config document that lives in
	// the same collection as plans. The runner loads it by name to resolve server URLs,
	// auth headers, and other transport infrastructure. Managed by admin UI, not n8n.
	PlanTypeTransportConfig = "TRANSPORT_CONFIG"
)

// TransportConfig is decoded from a TRANSPORT_CONFIG plan document's config field.
// Managed by admin UI / config API. Stored in same MongoDB collection as plans.
// Runner loads it lazily via PlanProvider and caches it.
type TransportConfig struct {
	TransportKey  string                   `json:"transportKey"`
	DisplayName   string                   `json:"displayName,omitempty"`
	DeviceTypes   []int                    `json:"deviceTypes,omitempty"`
	Auth          string                   `json:"auth"`                    // Authorization header value (e.g. "Basic YWRt...")
	Accept        string                   `json:"accept,omitempty"`        // Accept header (default: "application/json")
	Timeout       int                      `json:"timeout,omitempty"`       // seconds (default: 10)
	Protocol      string                   `json:"protocol,omitempty"`      // "https" or "http" (default: "https")
	Servers       map[string]Endpoint    `json:"servers"`                 // connection key → primary/secondary URLs
	ConnectionMap map[string]string        `json:"connectionMap,omitempty"` // input value → connection key (e.g. "FTN" → "MB", "*" → default)
}

// Endpoint holds primary + secondary server addresses for a region.
type Endpoint struct {
	Primary   string `json:"primary"`
	Secondary string `json:"secondary,omitempty"`
}

// resolveServer returns the full base URL for a given input value (e.g. parent).
// ConnectionMap maps input values to server keys: {"FTN": "MB", "*": "MN"}.
// "*" is the wildcard/default. If no map entry matches, tries the first available server.
func (tc *TransportConfig) resolveServer(connectionInput string) (primary, secondary string) {
	protocol := tc.Protocol
	if protocol == "" {
		protocol = "https"
	}

	// Resolve connection key from input
	connKey := ""
	if mapped, ok := tc.ConnectionMap[connectionInput]; ok {
		connKey = mapped
	} else if fallback, ok := tc.ConnectionMap["*"]; ok {
		connKey = fallback
	}

	pair, ok := tc.Servers[connKey]
	if !ok {
		// Fallback: try first available server
		for _, p := range tc.Servers {
			pair = p
			break
		}
	}

	primary = protocol + "://" + pair.Primary
	if pair.Secondary != "" {
		secondary = protocol + "://" + pair.Secondary
	}
	return
}

func (tc *TransportConfig) timeoutDuration() time.Duration {
	if tc.Timeout > 0 {
		return time.Duration(tc.Timeout) * time.Second
	}
	return 10 * time.Second
}

func (tc *TransportConfig) acceptHeader() string {
	if tc.Accept != "" {
		return tc.Accept
	}
	return "application/json"
}

// planConfig is decoded from a REST_PROPERTY plan's Config field.
type planConfig struct {
	DeviceTypes []int  `json:"deviceTypes,omitempty"`
	PlanKey     string `json:"planKey"`
	Transport   string `json:"transport"` // name reference to TRANSPORT_CONFIG doc (e.g. "nokia")
}

func decodePlanConfig(plan *maestro.Plan) planConfig {
	var cfg planConfig
	if len(plan.Config) > 0 {
		json.Unmarshal(plan.Config, &cfg)
	}
	return cfg
}

// Runner implements orchestrator.Runner for REST-based plan execution.
//
// Architecture:
//   - Plan graph builds a relative URL path + response field + optional transform
//   - Runner loads transport config (auth, servers) from a TRANSPORT_CONFIG document
//     in the same MongoDB collection as plans (via PlanProvider)
//   - Runner resolves server from parent input + transport config region map
//   - Runner makes HTTP call with proper headers, failover to secondary server
//   - Response caching: RunBatch groups plans by URL. TX+RX share one HTTP call.
//
// The runner has zero vendor-specific code. All vendor specifics live in:
//   - Plans (URL paths, response field names) — authored in n8n
//   - Transport config (auth, servers) — managed by admin UI
type Runner struct {
	eng        *engine.Engine
	httpClient *http.Client
	plans      orchestrator.PlanProvider // for loading TRANSPORT_CONFIG docs
	tracer     trace.Tracer

	mu             sync.RWMutex
	transportCache map[string]*TransportConfig // transport name → cached config
}

// NewRunner creates a REST runner.
//   - httpClient: stdlib HTTP client (shared, no vendor-specific config)
//   - plans: PlanProvider for lazy-loading transport configs from same MongoDB collection.
//     Can be nil during tests — runner will fail on first REST call if transport is needed.
//
// Outgoing URLs are traced via OTel span attributes + events so operators can
// reproduce production failures from trace data without inflating app logs.
func NewRunner(httpClient *http.Client, plans orchestrator.PlanProvider) *Runner {
	eng := engine.New()
	eng.RegisterStandardHandlers()
	return &Runner{
		eng:            eng,
		httpClient:     httpClient,
		plans:          plans,
		tracer:         otel.Tracer("rest-runner"),
		transportCache: make(map[string]*TransportConfig),
	}
}

func (r *Runner) Run(ctx context.Context, plan *maestro.Plan, action string, scope int, inputs map[string]any) (any, error) {
	_, span := r.tracer.Start(ctx, "REST Run")
	defer span.End()

	switch plan.Type {
	case PlanTypeRESTProperty:
		return r.execRESTProperty(ctx, span, plan, scope, inputs)
	case PlanTypeTransportConfig:
		// Transport configs are not executed — they're loaded by reference.
		return nil, nil
	default:
		// Non-REST plans: execute via engine (e.g. future chain types)
		return r.execPlan(plan, inputs)
	}
}

func (r *Runner) RunBatch(ctx context.Context, plans map[string]*maestro.Plan, action string, scope int, inputs map[string]any) (map[string]any, error) {
	_, span := r.tracer.Start(ctx, "REST RunBatch")
	defer span.End()

	results := make(map[string]any)

	// Phase 1: Execute plan graphs to get URL path + field info for each key.
	// Per-key failures here become error values in results (partial-success
	// semantics) so the rest of the batch can still proceed.
	type restTarget struct {
		URLPath       string
		ResponseField string
		Transform     string
		Transport     string // transport config name
	}
	targets := make(map[string]restTarget)
	var nonRESTKeys []string

	for key, plan := range plans {
		if plan.Type == PlanTypeRESTProperty {
			state := maestro.NewState(inputs)
			result, err := r.eng.Execute(plan, state)
			if err != nil {
				results[key] = fmt.Errorf("rest plan %s: %w", key, err)
				continue
			}
			if result.Status != maestro.StatusCompleted {
				results[key] = fmt.Errorf("rest plan %s did not complete (status: %s)", key, result.Status)
				continue
			}
			urlPath, responseField, transform, err := extractRESTParams(result.PrimitiveValue)
			if err != nil {
				results[key] = fmt.Errorf("rest params %s: %w", key, err)
				continue
			}
			cfg := decodePlanConfig(plan)
			targets[key] = restTarget{
				URLPath:       urlPath,
				ResponseField: responseField,
				Transform:     transform,
				Transport:     cfg.Transport,
			}
		} else if plan.Type == PlanTypeTransportConfig {
			// Skip transport config docs — not executed
			continue
		} else {
			nonRESTKeys = append(nonRESTKeys, key)
		}
	}

	// Phase 2: Load transport config (once per transport name in this batch).
	// Transport load failures are per-transport: every key that needed that
	// transport gets flagged; other transports can still succeed.
	transportConfigs := make(map[string]*TransportConfig)
	transportLoadErrs := make(map[string]error)
	for _, t := range targets {
		if t.Transport == "" || transportConfigs[t.Transport] != nil || transportLoadErrs[t.Transport] != nil {
			continue
		}
		tc, err := r.loadTransport(ctx, scope, t.Transport)
		if err != nil {
			transportLoadErrs[t.Transport] = err
			continue
		}
		transportConfigs[t.Transport] = tc
	}

	// Phase 3: Resolve full URLs and group by URL for deduplication.
	parent, _ := inputs["parent"].(string)

	type resolvedTarget struct {
		FullURL       string
		ResponseField string
		Transform     string
	}
	resolved := make(map[string]resolvedTarget)

	for key, t := range targets {
		if loadErr, bad := transportLoadErrs[t.Transport]; bad {
			results[key] = fmt.Errorf("transport %q: %w", t.Transport, loadErr)
			continue
		}
		tc := transportConfigs[t.Transport]
		if tc == nil {
			results[key] = fmt.Errorf("no transport config for %q (plan %s)", t.Transport, key)
			continue
		}
		primary, _ := tc.resolveServer(parent)
		fullURL := primary + t.URLPath
		resolved[key] = resolvedTarget{
			FullURL:       fullURL,
			ResponseField: t.ResponseField,
			Transform:     t.Transform,
		}
	}

	// Phase 4: Deduplicate by URL, fetch, extract.
	type cachedResponse struct {
		body map[string]any
		err  error
	}
	urlCache := make(map[string]*cachedResponse)

	// Pick the first transport config for HTTP settings (all REST plans in one batch
	// should share the same transport — they're for the same device type).
	var batchTC *TransportConfig
	for _, tc := range transportConfigs {
		batchTC = tc
		break
	}

	if batchTC != nil && r.httpClient != nil {
		// Collect unique URLs
		uniqueURLs := make(map[string]struct{})
		for _, rt := range resolved {
			uniqueURLs[rt.FullURL] = struct{}{}
		}

		span.AddEvent(fmt.Sprintf("REST Batch: %d targets, %d unique URLs", len(resolved), len(uniqueURLs)))

		// Fetch each unique URL
		for url := range uniqueURLs {
			body, err := r.doHTTPGet(ctx, span, url, batchTC)
			urlCache[url] = &cachedResponse{body: body, err: err}

			// Failover: if primary failed, try secondary
			if err != nil {
				_, secondary := batchTC.resolveServer(parent)
				if secondary != "" {
					// Replace primary prefix with secondary
					altURL := secondary + url[strings.Index(url[8:], "/")+8:] // skip "https://" to find path
					span.AddEvent("failover_to_secondary", trace.WithAttributes(
						attribute.String("secondary.url", altURL),
					))
					body, err = r.doHTTPGet(ctx, span, altURL, batchTC)
					urlCache[url] = &cachedResponse{body: body, err: err}
				}
			}
		}

		// Extract fields from cached responses. Per-URL HTTP failures and
		// per-key missing-field conditions become error values in results —
		// the batch still returns nil so other keys can succeed.
		for key, rt := range resolved {
			cached := urlCache[rt.FullURL]
			if cached.err != nil {
				results[key] = fmt.Errorf("rest get %s: %w", key, cached.err)
				span.AddEvent(fmt.Sprintf("  %s: HTTP failed — %v", key, cached.err))
				continue
			}
			val, ok := cached.body[rt.ResponseField]
			if !ok {
				available := make([]string, 0, len(cached.body))
				for k := range cached.body {
					available = append(available, k)
				}
				results[key] = fmt.Errorf("response field %q not found in response (available: %v)", rt.ResponseField, available)
				span.AddEvent(fmt.Sprintf("  %s: field %q not in response (keys: %v)", key, rt.ResponseField, available))
				continue
			}
			results[key] = applyTransform(val, rt.Transform)
			span.AddEvent(fmt.Sprintf("  %s: %v", key, results[key]))
		}
	}

	// Phase 5: Execute non-REST plans via engine
	for _, key := range nonRESTKeys {
		val, err := r.execPlan(plans[key], inputs)
		if err != nil {
			return nil, fmt.Errorf("exec %s: %w", key, err)
		}
		results[key] = val
	}

	return results, nil
}

func (r *Runner) SupportsBatch() bool {
	return r.httpClient != nil
}

func (r *Runner) Contract() *orchestrator.RunnerContract {
	return &orchestrator.RunnerContract{
		Name:           "rest",
		AllowedActions: []string{"GET"},
		Inputs: []orchestrator.ContractInput{
			{Key: "hostname", Type: "string", Required: true, NonEmpty: true, Description: "OLT hostname (used by plan graph to build URL path)"},
			{Key: "parent", Type: "string", Required: true, NonEmpty: true, Description: "Region parent (FTN/etc.) — used to resolve server from transport config"},
		},
		PlanIO: map[string]orchestrator.PlanTypeIO{
			PlanTypeRESTProperty: {
				DefaultAction:   "GET",
				ContextInputs:   nil,
				RequiredOutputs: []string{"url", "responseField"},
			},
			// TRANSPORT_CONFIG is declared so the orchestrator knows this runner
			// "handles" it (skips it during execution — it's config, not executable).
			PlanTypeTransportConfig: {
				DefaultAction:   "",
				RequiredOutputs: nil,
			},
		},
	}
}

// ClearTransportCache invalidates all cached transport configs.
// Called when plan cache is cleared (same lifecycle).
func (r *Runner) ClearTransportCache() {
	r.mu.Lock()
	r.transportCache = make(map[string]*TransportConfig)
	r.mu.Unlock()
}

// --- Internal methods ---

func (r *Runner) execRESTProperty(ctx context.Context, span trace.Span, plan *maestro.Plan, scope int, inputs map[string]any) (any, error) {
	// Execute plan graph to resolve URL path and response field
	state := maestro.NewState(inputs)
	result, err := r.eng.Execute(plan, state)
	if err != nil {
		return nil, fmt.Errorf("rest plan failed: %w", err)
	}
	if result.Status != maestro.StatusCompleted {
		return nil, fmt.Errorf("rest plan did not complete (status: %s)", result.Status)
	}

	urlPath, responseField, transform, err := extractRESTParams(result.PrimitiveValue)
	if err != nil {
		return nil, fmt.Errorf("rest param extract failed: %w", err)
	}

	// Load transport config
	cfg := decodePlanConfig(plan)
	tc, err := r.loadTransport(ctx, scope, cfg.Transport)
	if err != nil {
		return nil, fmt.Errorf("transport %q: %w", cfg.Transport, err)
	}

	// Resolve server from parent input
	parent, _ := inputs["parent"].(string)
	primary, secondary := tc.resolveServer(parent)
	fullURL := primary + urlPath

	span.SetAttributes(
		attribute.String("rest.url", fullURL),
		attribute.String("rest.transport", cfg.Transport),
		attribute.String("rest.responseField", responseField),
	)

	// HTTP GET with failover
	body, err := r.doHTTPGet(ctx, span, fullURL, tc)
	if err != nil && secondary != "" {
		altURL := secondary + urlPath
		span.AddEvent("failover_to_secondary")
		body, err = r.doHTTPGet(ctx, span, altURL, tc)
	}
	if err != nil {
		return nil, err
	}

	val, ok := body[responseField]
	if !ok {
		return nil, fmt.Errorf("response field %q not found in response", responseField)
	}

	val = applyTransform(val, transform)
	span.AddEvent(fmt.Sprintf("REST %s → %v", responseField, val))
	return val, nil
}

// loadTransport loads a TRANSPORT_CONFIG document by name, with in-memory caching.
func (r *Runner) loadTransport(ctx context.Context, scope int, transportName string) (*TransportConfig, error) {
	if transportName == "" {
		return nil, fmt.Errorf("plan has no transport config name (config.transport is empty)")
	}

	// Check cache
	r.mu.RLock()
	if tc, ok := r.transportCache[transportName]; ok {
		r.mu.RUnlock()
		return tc, nil
	}
	r.mu.RUnlock()

	// Load from PlanProvider
	if r.plans == nil {
		return nil, fmt.Errorf("no PlanProvider configured — cannot load transport %q", transportName)
	}

	docKey := "__TRANSPORT_" + strings.ToUpper(transportName)
	plan, err := r.plans.GetPlanByKey(ctx, scope, docKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load transport config %q: %w", docKey, err)
	}

	var tc TransportConfig
	if len(plan.Config) > 0 {
		if err := json.Unmarshal(plan.Config, &tc); err != nil {
			return nil, fmt.Errorf("failed to decode transport config %q: %w", docKey, err)
		}
	}

	// Cache it
	r.mu.Lock()
	r.transportCache[transportName] = &tc
	r.mu.Unlock()

	return &tc, nil
}

// doHTTPGet performs a single HTTP GET with headers from transport config.
//
// The outgoing URL is set as a span attribute BEFORE dispatching so traces
// always carry the full URL even when the request errors out. Errors attach a
// body snippet as an event so operators can reproduce from trace data without
// inflating application logs with every request.
func (r *Runner) doHTTPGet(ctx context.Context, span trace.Span, url string, tc *TransportConfig) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, tc.timeoutDuration())
	defer cancel()

	// Set URL on the span up front so every failure mode carries it.
	span.SetAttributes(attribute.String("http.url", url))
	span.AddEvent("rest.get", trace.WithAttributes(attribute.String("http.url", url)))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rest: failed to create request: %w", err)
	}
	req.Header.Set("Accept", tc.acceptHeader())
	if tc.Auth != "" {
		req.Header.Set("Authorization", tc.Auth)
	}

	start := time.Now()
	resp, err := r.httpClient.Do(req)
	duration := time.Since(start)
	span.SetAttributes(attribute.Float64("http.duration_ms", float64(duration.Milliseconds())))

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request failed")
		return nil, fmt.Errorf("rest: request failed: %w", err)
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		snippet := string(body)
		span.AddEvent("http.error_response", trace.WithAttributes(
			attribute.Int("http.status_code", resp.StatusCode),
			attribute.String("http.response_body_snippet", snippet),
		))
		return nil, fmt.Errorf("rest: server returned status %d: %s", resp.StatusCode, snippet)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rest: failed to read response body: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("rest: failed to decode JSON: %w, body: %.512s", err, string(body))
	}

	span.AddEvent("rest.ok", trace.WithAttributes(
		attribute.Int("http.status_code", resp.StatusCode),
		attribute.Int64("http.duration_ms", duration.Milliseconds()),
	))

	return result, nil
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

// extractRESTParams extracts URL path, response field, and optional transform
// from the plan graph's execution result.
func extractRESTParams(value any) (urlPath, responseField, transform string, err error) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", "", "", fmt.Errorf("expected map result from REST plan, got %T", value)
	}
	urlVal, ok := m["url"]
	if !ok {
		return "", "", "", fmt.Errorf("REST plan result missing 'url' field")
	}
	urlPath = fmt.Sprintf("%v", urlVal)

	fieldVal, ok := m["responseField"]
	if !ok {
		return "", "", "", fmt.Errorf("REST plan result missing 'responseField' field")
	}
	responseField = fmt.Sprintf("%v", fieldVal)

	if t, ok := m["transform"]; ok {
		transform = fmt.Sprintf("%v", t)
	}

	return urlPath, responseField, transform, nil
}

// applyTransform applies a simple numeric transform to a value.
// Supported: "div256" (temperature), "div10" (power dBm), "div100".
func applyTransform(val any, transform string) any {
	if transform == "" {
		return val
	}

	var num float64
	switch v := val.(type) {
	case float64:
		num = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return val
		}
		num = f
	case int:
		num = float64(v)
	case int64:
		num = float64(v)
	default:
		return val
	}

	switch transform {
	case "div256":
		return num / 256.0
	case "div10":
		return num / 10.0
	case "div100":
		return num / 100.0
	default:
		return val
	}
}
