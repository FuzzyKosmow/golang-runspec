package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FuzzyKosmow/golang-runspec"
	"github.com/FuzzyKosmow/golang-runspec/engine"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

// shortCircuitError signals that a guard produced a final result,
// skipping runner execution entirely.
type shortCircuitError struct {
	Value any
}

func (e *shortCircuitError) Error() string { return "short-circuit" }

// Orchestrator is the reusable orchestration engine.
// It handles: plan fetch → dependency discovery → guard evaluation →
// delegation to runner executor → chain execution → result assembly.
//
// Runners (SNMP, REST, Kafka, Batch Scanner, etc.) provide a Runner.
// Multiple runners can be registered by plan type via RegisterRunner.
// The Orchestrator handles everything else.
type Orchestrator struct {
	plans   PlanProvider
	runner  Runner            // default runner (backward compat)
	runners map[string]Runner // plan.Type → runner (optional, checked first)
	gate    CapabilityGate    // optional
	engine  *engine.Engine
}

// NewOrchestrator creates an orchestrator with a default runner.
// gate can be nil to skip capability validation (all items allowed).
// Additional runners can be registered via RegisterRunner for specific plan types.
func NewOrchestrator(plans PlanProvider, runner Runner, gate CapabilityGate) *Orchestrator {
	eng := engine.New()
	eng.RegisterStandardHandlers()

	return &Orchestrator{
		plans:   plans,
		runner:  runner,
		runners: make(map[string]Runner),
		gate:    gate,
		engine:  eng,
	}
}

// RegisterRunner registers a runner for all plan types declared in its Contract().PlanIO.
// When executing a plan, the orchestrator checks this registry first (by plan.Type),
// falling back to the default runner if no match is found.
// This allows multiple transport types (SNMP, REST, gRPC, etc.) to coexist.
//
// If the runner has no contract or no PlanIO entries, it is not registered.
// Duplicate plan type registrations overwrite silently (last writer wins).
func (r *Orchestrator) RegisterRunner(runner Runner) {
	contract := runner.Contract()
	if contract == nil {
		return
	}
	for planType := range contract.PlanIO {
		r.runners[planType] = runner
	}
}

// runnerForPlan returns the runner to use for a given plan.
// Checks the plan type registry first, falls back to the default runner.
func (r *Orchestrator) runnerForPlan(plan *maestro.Plan) Runner {
	if runner, ok := r.runners[plan.Type]; ok {
		return runner
	}
	return r.runner
}

// Run orchestrates the full execution for multiple keys within a scope.
// This is the main entry point — services call this with their inputs.
// "scope" and "keys" are domain-specific: for SNMP scope=deviceType, keys=property names.
// "action" is the runner action verb (GET, SET, WALK, EXECUTE).
//
// Flow:
//  1. Validate action against runner contract
//  2. Validate keys against capability gate (optional)
//  3. Fetch plans for all requested keys
//  4. Discover dependencies from guards
//  5. Resolve dependencies concurrently (via runner)
//  6. Per key: evaluate guards → run → chains
//  7. Return results with full tracing for audit
func (r *Orchestrator) Run(ctx context.Context, action string, scope int, keys []string, inputs map[string]any) (map[string]Result, error) {
	span := trace.SpanFromContext(ctx)
	tStart := time.Now()

	// Timing breakdown for audit
	var durGate, durPlans, durDepsDiscovery, durDeps time.Duration
	var durGuards, durExec, durChains time.Duration

	results := make(map[string]Result)

	if len(keys) == 0 {
		return results, nil
	}

	// Validate default runner contract (top-level sanity check).
	// Per-plan runner contracts are validated at execution time when runners are registered.
	if len(r.runners) == 0 {
		if contract := r.runner.Contract(); contract != nil {
			if !contract.IsActionAllowed(action) {
				return nil, fmt.Errorf("action %q not allowed by runner %q (allowed: %v)", action, contract.Name, contract.AllowedActions)
			}
			if warnings := contract.Validate(inputs); len(warnings) > 0 {
				span.AddEvent(fmt.Sprintf("Contract WARN: runner=%s %s", contract.Name, strings.Join(warnings, "; ")))
			}
		}
	}

	span.AddEvent(fmt.Sprintf("Orchestrator: %s scope=%d — %d keys [%s]",
		action, scope, len(keys), strings.Join(keys, ", ")), trace.WithAttributes(
		attribute.Int("scope", scope),
		attribute.String("action", action),
		attribute.Int("keyCount", len(keys)),
		attribute.StringSlice("keys", keys),
	))

	// === Phase 0: Gate validation ===
	tGate := time.Now()
	allowedMap := make(map[string]bool)
	if r.gate != nil {
		allowed, err := r.gate.GetAllowed(ctx, scope)
		if err != nil {
			span.AddEvent(fmt.Sprintf("Gate ERROR: %s", truncate(err.Error(), 80)))
			return nil, fmt.Errorf("capability gate check failed: %w", err)
		}
		allowedMap = allowed
	}

	var validKeys []string
	var unsupported []string
	for _, key := range keys {
		if r.gate != nil && !allowedMap[key] {
			results[key] = Result{
				Name:   key,
				Status: StatusUnsupported,
				Error:  fmt.Sprintf("key %s not allowed for scope %d", key, scope),
			}
			unsupported = append(unsupported, key)
			continue
		}
		validKeys = append(validKeys, key)
	}
	durGate = time.Since(tGate)

	if len(unsupported) > 0 {
		span.AddEvent(fmt.Sprintf("Gate: %d allowed, %d unsupported [%s]",
			len(validKeys), len(unsupported), strings.Join(unsupported, ", ")), trace.WithAttributes(
			attribute.Int("valid", len(validKeys)),
			attribute.StringSlice("unsupported", unsupported),
			attribute.Int64("duration_ms", durGate.Milliseconds()),
		))
	}

	if len(validKeys) == 0 {
		return results, nil
	}

	// === Phase 1: Fetch plans ===
	tPlans := time.Now()
	planCache := make(map[string]*maestro.Plan)
	plans, err := r.plans.GetPlans(ctx, scope, validKeys)
	if err != nil {
		span.AddEvent(fmt.Sprintf("Plans ERROR: %s", truncate(err.Error(), 80)))
		return nil, fmt.Errorf("plan fetch failed: %w", err)
	}
	for k, plan := range plans {
		planCache[k] = plan
	}

	var missingPlans []string
	for _, key := range validKeys {
		if _, ok := planCache[key]; !ok {
			results[key] = Result{
				Name:   key,
				Status: StatusError,
				Error:  fmt.Sprintf("no plan found for key %s", key),
			}
			missingPlans = append(missingPlans, key)
		}
	}
	durPlans = time.Since(tPlans)

	span.AddEvent(fmt.Sprintf("Plans: %d fetched, %d missing (%dms)",
		len(planCache), len(missingPlans), durPlans.Milliseconds()), trace.WithAttributes(
		attribute.Int("fetched", len(planCache)),
		attribute.Int("missing", len(missingPlans)),
		attribute.Int64("duration_ms", durPlans.Milliseconds()),
	))

	// === Phase 2: Discover dependencies from guards ===
	tDepsDisc := time.Now()
	depSet := make(map[string]struct{})
	for _, key := range validKeys {
		plan, ok := planCache[key]
		if !ok {
			continue
		}
		for _, dep := range ExtractDependencies(plan, action) {
			if dep != key {
				depSet[dep] = struct{}{}
			}
		}
	}

	// Fetch dependency plans
	for dep := range depSet {
		if _, ok := planCache[dep]; ok {
			continue
		}
		if r.gate != nil && !allowedMap[dep] {
			continue
		}
		depPlan, err := r.plans.GetPlan(ctx, scope, dep)
		if err != nil {
			span.AddEvent(fmt.Sprintf("Dep %s: plan not found — skipped", dep))
			continue
		}
		planCache[dep] = depPlan
	}
	durDepsDiscovery = time.Since(tDepsDisc)

	if len(depSet) > 0 {
		depList := make([]string, 0, len(depSet))
		for d := range depSet {
			depList = append(depList, d)
		}
		span.AddEvent(fmt.Sprintf("Dependencies: %d found [%s]",
			len(depList), strings.Join(depList, ", ")), trace.WithAttributes(
			attribute.StringSlice("dependencies", depList),
			attribute.Int("count", len(depList)),
			attribute.Int64("duration_ms", durDepsDiscovery.Milliseconds()),
		))
	}

	// === Phase 3: Resolve dependencies concurrently ===
	tDeps := time.Now()
	var mu sync.Mutex
	valueCache := make(map[string]any)
	failureCache := make(map[string]error)

	// Dependencies are always resolved with the default action for their plan type.
	depAction := r.defaultActionForDeps()

	g, gCtx := errgroup.WithContext(ctx)
	for depName := range depSet {
		depName := depName
		g.Go(func() error {
			mu.Lock()
			_, done := valueCache[depName]
			mu.Unlock()
			if done {
				span.AddEvent(fmt.Sprintf("Dep %s: cache hit", depName))
				return nil
			}

			plan, ok := planCache[depName]
			if !ok {
				return nil
			}

			tDep := time.Now()
			val, err := r.runnerForPlan(plan).Run(gCtx, plan, depAction, scope, inputs)
			if err != nil {
				mu.Lock()
				failureCache[depName] = err
				mu.Unlock()
				span.AddEvent(fmt.Sprintf("Dep %s: FAILED — %s", depName, truncate(err.Error(), 80)))
				return nil // don't cancel other deps
			}

			// Apply chains if configured (e.g., RUN_PLAN maps raw → human value).
			// Critical: dependency checks compare against POST-PROCESSED values (e.g., "Online" not "1").
			if plan.HasChains(depAction) {
				tPost := time.Now()
				val, err = r.applyChains(gCtx, plan, depAction, scope, inputs, val, nil)
				if err != nil {
					mu.Lock()
					failureCache[depName] = fmt.Errorf("dependency %s chains failed: %w", depName, err)
					mu.Unlock()
					span.AddEvent(fmt.Sprintf("Dep %s: chain failed — %s", depName, truncate(err.Error(), 80)))
					return nil
				}
				span.AddEvent(fmt.Sprintf("Dep %s: chain applied (%dms)", depName, time.Since(tPost).Milliseconds()))
			}

			mu.Lock()
			valueCache[depName] = val
			mu.Unlock()

			span.AddEvent(fmt.Sprintf("Dep %s: resolved = %s (%dms)",
				depName, truncate(FormatValue(val), 40), time.Since(tDep).Milliseconds()))
			return nil
		})
	}
	g.Wait()
	durDeps = time.Since(tDeps)

	// === Phase 4: Per-key guard evaluation ===
	type keyTask struct {
		Plan       *maestro.Plan
		Logs       []string
		Guards     []GuardResult
		Injections map[string]any
	}
	pendingExec := make(map[string]keyTask)

	tGuard := time.Now()
	for _, key := range validKeys {
		if _, done := results[key]; done {
			continue
		}

		plan, ok := planCache[key]
		if !ok {
			continue
		}

		var logs []string

		// Check if already resolved as a dependency
		mu.Lock()
		if val, ok := valueCache[key]; ok {
			mu.Unlock()
			results[key] = Result{Name: key, Status: StatusSuccess, Value: val, Logs: logs}
			span.AddEvent(fmt.Sprintf("  %s: from dep cache", key))
			continue
		}
		if err, ok := failureCache[key]; ok {
			mu.Unlock()
			results[key] = Result{Name: key, Status: StatusError, Error: err.Error(), Logs: logs}
			continue
		}
		mu.Unlock()

		guardResults, injections, err := r.applyGuards(ctx, plan, action, scope, planCache, &mu, valueCache, failureCache, inputs, allowedMap)
		if err != nil {
			// Check for short-circuit (OVERWRITE with expect+result match)
			var sc *shortCircuitError
			if errors.As(err, &sc) {
				results[key] = Result{
					Name: key, Status: StatusSuccess, Value: sc.Value,
					Logs: logs, GuardResults: guardResults,
				}
				span.AddEvent(fmt.Sprintf("  %s: SHORT-CIRCUIT → %s", key, truncate(FormatValue(sc.Value), 40)))
				continue
			}

			AppendLog(&logs, fmt.Sprintf("Guard failed: %v", err))
			results[key] = Result{
				Name:         key,
				Status:       StatusSkipped,
				Error:        err.Error(),
				Logs:         logs,
				GuardResults: guardResults,
			}
			span.AddEvent(fmt.Sprintf("  %s: BLOCKED — %s", key, truncate(err.Error(), 80)))
			continue
		}

		if len(injections) > 0 {
			injKeys := make([]string, 0, len(injections))
			for k := range injections {
				injKeys = append(injKeys, k)
			}
			span.AddEvent(fmt.Sprintf("  %s: injected [%s]", key, strings.Join(injKeys, ", ")))
		}

		pendingExec[key] = keyTask{Plan: plan, Logs: logs, Guards: guardResults, Injections: injections}
	}
	durGuards = time.Since(tGuard)

	// === Phase 5: Execute (batch or single, grouped by runner) ===
	tExec := time.Now()

	// Group pending plans by their runner so each runner can batch its own plans.
	type runnerGroup struct {
		runner Runner
		tasks  map[string]keyTask
	}
	groups := make(map[Runner]*runnerGroup)
	for key, task := range pendingExec {
		rn := r.runnerForPlan(task.Plan)
		g, ok := groups[rn]
		if !ok {
			g = &runnerGroup{runner: rn, tasks: make(map[string]keyTask)}
			groups[rn] = g
		}
		g.tasks[key] = task
	}

	for _, grp := range groups {
		if grp.runner.SupportsBatch() {
			batchPlans := make(map[string]*maestro.Plan)
			for key, task := range grp.tasks {
				batchPlans[key] = task.Plan
			}

			span.AddEvent(fmt.Sprintf("Execute: batch %s — %d plans", action, len(batchPlans)))

			batchResults, err := grp.runner.RunBatch(ctx, batchPlans, action, scope, inputs)
			if err != nil {
				for key, task := range grp.tasks {
					results[key] = Result{
						Name: key, Status: StatusError,
						Error: fmt.Sprintf("batch execution failed: %v", err),
						Logs: task.Logs, GuardResults: task.Guards,
					}
				}
				span.AddEvent(fmt.Sprintf("Execute: batch FAILED — %s", truncate(err.Error(), 80)))
			} else {
				tChain := time.Now()
				for key, rawVal := range batchResults {
					task := grp.tasks[key]

					// Per-key partial-success: runners may surface individual
					// key failures as error values in the result map while
					// letting sibling keys succeed. Check that first.
					if perKeyErr, ok := rawVal.(error); ok {
						results[key] = Result{
							Name: key, Status: StatusError,
							Error: fmt.Sprintf("execution failed: %v", perKeyErr),
							Logs: task.Logs, GuardResults: task.Guards,
						}
						span.AddEvent(fmt.Sprintf("  %s: FAILED — %s", key, truncate(perKeyErr.Error(), 60)))
						continue
					}

					val := rawVal

					if task.Plan.HasChains(action) {
						tPP := time.Now()
						val, err = r.applyChains(ctx, task.Plan, action, scope, inputs, rawVal, task.Injections)
						if err != nil {
							AppendLog(&task.Logs, fmt.Sprintf("Chain failed: %v", err))
							results[key] = Result{
								Name: key, Status: StatusError,
								Error: fmt.Sprintf("chain failed: %v", err),
								Logs: task.Logs, GuardResults: task.Guards,
							}
							span.AddEvent(fmt.Sprintf("  %s: chain FAILED — %s", key, truncate(err.Error(), 60)))
							continue
						}
						span.AddEvent(fmt.Sprintf("  %s: chain OK (%dms)", key, time.Since(tPP).Milliseconds()))
					}

					results[key] = Result{
						Name: key, Status: StatusSuccess, Value: val,
						Logs: task.Logs, GuardResults: task.Guards,
					}
				}
				durChains = time.Since(tChain)
			}
		} else {
			// Single execution for this runner's plans
			for key, task := range grp.tasks {
				tSingle := time.Now()
				val, err := grp.runner.Run(ctx, task.Plan, action, scope, inputs)
				if err != nil {
					results[key] = Result{
						Name: key, Status: StatusError,
						Error: fmt.Sprintf("execution failed: %v", err),
						Logs: task.Logs, GuardResults: task.Guards,
					}
					span.AddEvent(fmt.Sprintf("  %s: exec FAILED — %s", key, truncate(err.Error(), 60)))
					continue
				}

				if task.Plan.HasChains(action) {
					tPP := time.Now()
					val, err = r.applyChains(ctx, task.Plan, action, scope, inputs, val, task.Injections)
					if err != nil {
						results[key] = Result{
							Name: key, Status: StatusError,
							Error: fmt.Sprintf("chain failed: %v", err),
							Logs: task.Logs, GuardResults: task.Guards,
						}
						continue
					}
					span.AddEvent(fmt.Sprintf("  %s: chain OK (%dms)", key, time.Since(tPP).Milliseconds()))
				}

				results[key] = Result{
					Name: key, Status: StatusSuccess, Value: val,
					Logs: task.Logs, GuardResults: task.Guards,
				}

				span.AddEvent(fmt.Sprintf("  %s: OK (%dms)", key, time.Since(tSingle).Milliseconds()))
			}
		}
	}
	durExec = time.Since(tExec)

	// === Summary: timing breakdown for optimization audit ===
	var successCount, errorCount, skippedCount, unsupportedCount int
	for _, r := range results {
		switch r.Status {
		case StatusSuccess:
			successCount++
		case StatusError:
			errorCount++
		case StatusSkipped:
			skippedCount++
		case StatusUnsupported:
			unsupportedCount++
		}
	}

	totalMs := time.Since(tStart).Milliseconds()
	span.AddEvent(fmt.Sprintf("Orchestrator: DONE — %d ok, %d err, %d skip, %d unsup (%dms) [gate=%dms plans=%dms deps=%dms guards=%dms exec=%dms chains=%dms]",
		successCount, errorCount, skippedCount, unsupportedCount, totalMs,
		durGate.Milliseconds(), durPlans.Milliseconds(), durDeps.Milliseconds(),
		durGuards.Milliseconds(), durExec.Milliseconds(), durChains.Milliseconds()),
		trace.WithAttributes(
			attribute.Int("scope", scope),
			attribute.String("action", action),
			attribute.Int("total", len(keys)),
			attribute.Int("success", successCount),
			attribute.Int("error", errorCount),
			attribute.Int("skipped", skippedCount),
			attribute.Int("unsupported", unsupportedCount),
			attribute.Int64("ms_total", totalMs),
		))

	return results, nil
}

// defaultActionForDeps returns the action to use when resolving dependencies.
// Dependencies are always resolved with their plan type's default action.
func (r *Orchestrator) defaultActionForDeps() string {
	// For now, dependencies are always GET (read the value to check against).
	// Future: could look up per-planType default from contract.
	return "GET"
}

// ExtractDependencies scans plan.RunSpec[action].Guards for dependency keys.
func ExtractDependencies(plan *maestro.Plan, action string) []string {
	spec := plan.GetActionSpec(action)
	if spec == nil {
		return nil
	}

	var deps []string
	seen := make(map[string]bool)

	for _, g := range spec.Guards {
		if g.Key != "" && !seen[g.Key] {
			deps = append(deps, g.Key)
			seen[g.Key] = true
		}
	}

	return deps
}

// applyGuards evaluates guards for a plan's action.
// Returns guard results, an injection map (for OVERWRITE), and error if a gate fails.
// Returns shortCircuitError if OVERWRITE matches expect+result (caller should use the value directly).
func (r *Orchestrator) applyGuards(
	ctx context.Context,
	plan *maestro.Plan,
	action string,
	scope int,
	planCache map[string]*maestro.Plan,
	mu *sync.Mutex,
	valueCache map[string]any,
	failureCache map[string]error,
	inputs map[string]any,
	allowedMap map[string]bool,
) ([]GuardResult, map[string]any, error) {
	span := trace.SpanFromContext(ctx)
	var guardResults []GuardResult
	injections := make(map[string]any)

	spec := plan.GetActionSpec(action)
	if spec == nil {
		return guardResults, injections, nil
	}

	for _, guard := range spec.Guards {
		key := guard.Key
		if key == "" {
			continue
		}

		val, depErr := r.resolveDependency(ctx, key, scope, planCache, mu, valueCache, failureCache, inputs, allowedMap)

		switch guard.Check {
		case "MUST_EQUAL":
			var p maestro.MustEqualParams
			json.Unmarshal(guard.Params, &p)

			if depErr != nil {
				guardResults = append(guardResults, GuardResult{
					Key: key, Check: "MUST_EQUAL", Status: StatusError,
					Message: fmt.Sprintf("dependency %s failed: %v", key, depErr),
				})
				if p.OnError == "WARN" {
					span.AddEvent(fmt.Sprintf("  Guard WARN (dep error): %s — %v", key, depErr))
					continue
				}
				return guardResults, injections, fmt.Errorf("dependency %s failed: %w", key, depErr)
			}

			got := FormatValue(val)
			match := false
			for _, expected := range p.Expect {
				if got == expected {
					match = true
					break
				}
			}

			if !match {
				msg := p.Message
				if msg == "" {
					msg = fmt.Sprintf("key %s expected %v got %s", key, p.Expect, got)
				}

				if p.OnFail == "WARN" {
					guardResults = append(guardResults, GuardResult{
						Key: key, Check: "MUST_EQUAL", Status: StatusSuccess,
						Expected: p.Expect, Actual: got,
						Message: msg + " (warning, continuing)",
					})
					span.AddEvent(fmt.Sprintf("  Guard WARN: %s=%s (expected %s)", key, got, strings.Join(p.Expect, ",")))
					continue
				}

				guardResults = append(guardResults, GuardResult{
					Key: key, Check: "MUST_EQUAL", Status: StatusSkipped,
					Expected: p.Expect, Actual: got, Message: msg,
				})
				span.AddEvent(fmt.Sprintf("  Guard BLOCKED: %s=%s (expected %s) → %s", key, got, strings.Join(p.Expect, ","), p.OnFail))
				return guardResults, injections, fmt.Errorf("%s", msg)
			}

			guardResults = append(guardResults, GuardResult{
				Key: key, Check: "MUST_EQUAL", Status: StatusSuccess,
				Expected: p.Expect, Actual: got,
				Message: fmt.Sprintf("key %s validated: %s", key, got),
			})

		case "MUST_EXIST":
			var p maestro.MustExistParams
			json.Unmarshal(guard.Params, &p)

			if depErr != nil {
				msg := p.Message
				if msg == "" {
					msg = fmt.Sprintf("required key %s not available: %v", key, depErr)
				}
				guardResults = append(guardResults, GuardResult{
					Key: key, Check: "MUST_EXIST", Status: StatusError,
					Message: msg,
				})
				if p.OnFail == "WARN" {
					span.AddEvent(fmt.Sprintf("  Guard WARN: %s must exist (missing)", key))
					continue
				}
				return guardResults, injections, fmt.Errorf("%s", msg)
			}
			guardResults = append(guardResults, GuardResult{
				Key: key, Check: "MUST_EXIST", Status: StatusSuccess,
				Actual: FormatValue(val),
				Message: fmt.Sprintf("key %s exists", key),
			})

		case "OVERWRITE":
			var p maestro.OverwriteParams
			json.Unmarshal(guard.Params, &p)

			if depErr != nil {
				msg := fmt.Sprintf("cannot resolve %s for injection: %v", key, depErr)
				guardResults = append(guardResults, GuardResult{
					Key: key, Check: "OVERWRITE", Status: StatusError,
					Message: msg,
				})
				if p.OnError == "WARN" || p.OnFail == "WARN" {
					span.AddEvent(fmt.Sprintf("  Guard WARN (dep error): %s — %v", key, depErr))
					continue
				}
				return guardResults, injections, fmt.Errorf("%s", msg)
			}

			got := FormatValue(val)
			alias := p.As
			if alias == "" {
				alias = key
			}

			// Short-circuit: if resolved value matches expect and result is set,
			// skip runner entirely and return the result value directly.
			if len(p.Expect) > 0 && p.Result != "" {
				for _, exp := range p.Expect {
					if got == exp {
						guardResults = append(guardResults, GuardResult{
							Key: key, Check: "OVERWRITE", Status: StatusSuccess,
							Expected: p.Expect, Actual: got,
							Message: fmt.Sprintf("short-circuit: %s=%s → result=%s", key, got, p.Result),
						})
						span.AddEvent(fmt.Sprintf("  Guard SHORT-CIRCUIT: %s=%s → %s", key, got, p.Result))
						return guardResults, injections, &shortCircuitError{Value: p.Result}
					}
				}
			}

			injections[alias] = got

			guardResults = append(guardResults, GuardResult{
				Key: key, Check: "OVERWRITE", Status: StatusSuccess,
				Actual: got,
				Message: fmt.Sprintf("injected %s as %s = %s", key, alias, got),
			})
			span.AddEvent(fmt.Sprintf("  Guard INJECT: %s → %s = %s", key, alias, truncate(got, 40)))
		}
	}

	return guardResults, injections, nil
}

// resolveDependency resolves a single dependency value, using cache when available.
// If not cached, executes the dependency plan (including its chains).
func (r *Orchestrator) resolveDependency(
	ctx context.Context,
	depKey string,
	scope int,
	planCache map[string]*maestro.Plan,
	mu *sync.Mutex,
	valueCache map[string]any,
	failureCache map[string]error,
	inputs map[string]any,
	allowedMap map[string]bool,
) (any, error) {
	span := trace.SpanFromContext(ctx)
	depAction := r.defaultActionForDeps()

	// Check cache first (fast path)
	mu.Lock()
	if val, ok := valueCache[depKey]; ok {
		mu.Unlock()
		span.AddEvent(fmt.Sprintf("Dep %s: cache hit", depKey))
		return val, nil
	}
	if err, ok := failureCache[depKey]; ok {
		mu.Unlock()
		return nil, err
	}
	mu.Unlock()

	// Need to resolve fresh
	depPlan, ok := planCache[depKey]
	if !ok {
		var err error
		depPlan, err = r.plans.GetPlan(ctx, scope, depKey)
		if err != nil {
			return nil, fmt.Errorf("dependency %s: plan not found: %w", depKey, err)
		}
		planCache[depKey] = depPlan
	}

	tDep := time.Now()
	val, err := r.runnerForPlan(depPlan).Run(ctx, depPlan, depAction, scope, inputs)
	if err != nil {
		mu.Lock()
		failureCache[depKey] = err
		mu.Unlock()
		return nil, err
	}

	// Apply chains if configured (e.g., RUN_PLAN maps raw → human value)
	if depPlan.HasChains(depAction) {
		val, err = r.applyChains(ctx, depPlan, depAction, scope, inputs, val, nil)
		if err != nil {
			wrappedErr := fmt.Errorf("dependency %s chains failed: %w", depKey, err)
			mu.Lock()
			failureCache[depKey] = wrappedErr
			mu.Unlock()
			return nil, wrappedErr
		}
	}

	mu.Lock()
	valueCache[depKey] = val
	mu.Unlock()

	span.AddEvent(fmt.Sprintf("Dep %s: resolved (%dms)", depKey, time.Since(tDep).Milliseconds()))

	return val, nil
}

// applyChains processes all chains for a plan's action result.
// Dispatches by step type: RUN_PLAN executes a sub-plan, future types (EMIT_EVENT, etc.) handled here.
// Returns the (potentially transformed) value after all chains are applied.
func (r *Orchestrator) applyChains(ctx context.Context, plan *maestro.Plan, action string, scope int, inputs map[string]any, rawValue any, injections map[string]any) (any, error) {
	span := trace.SpanFromContext(ctx)
	val := rawValue

	spec := plan.GetActionSpec(action)
	if spec == nil {
		return val, nil
	}

	for _, chain := range spec.Chains {
		switch chain.Do {
		case "RUN_PLAN":
			var p maestro.RunPlanParams
			if err := json.Unmarshal(chain.Params, &p); err != nil || p.PlanKey == "" {
				continue
			}

			postPlan, err := r.plans.GetPlanByKey(ctx, scope, p.PlanKey)
			if err != nil {
				span.AddEvent(fmt.Sprintf("  Chain: plan %s not found — skipped", p.PlanKey))
				continue // skip if plan not found
			}

			mergedInputs := CloneMap(inputs)

			// Determine the input key for the chain value by asking the runner
			// that CLAIMS the post-processing plan's type — not the parent plan's
			// runner. Per-runner POST_PROC types (v0.1.4+, e.g. POST_PROC_SNMP,
			// POST_PROC_REST) make this lookup unambiguous: each POST_PROC type is
			// registered by exactly one runner, which defines its ContextInputs.
			// Legacy POST_PROCESSING plans (unclaimed by any runner) fall back to
			// the default "result" key.
			resultKey := "result"
			if contract := r.runnerForPlan(postPlan).Contract(); contract != nil {
				if planIO, ok := contract.PlanIO[postPlan.Type]; ok && len(planIO.ContextInputs) > 0 {
					resultKey = planIO.ContextInputs[0].Key
				}
			}
			// Pass raw value to post-processing — don't coerce []byte (serial numbers, MACs)
			// since POST_PROC plans handle raw bytes via BYTES_TO_HEX transform.
			// Only coerce numeric types (int, float, json.Number → float64 for expressions).
			if _, isByteSlice := val.([]byte); isByteSlice {
				mergedInputs[resultKey] = val
			} else {
				mergedInputs[resultKey] = CoerceNumeric(val)
			}
			for k, v := range injections {
				mergedInputs[k] = v
			}

			state := maestro.NewState(mergedInputs)
			result, err := r.engine.Execute(postPlan, state)
			if err != nil {
				return nil, fmt.Errorf("chain RUN_PLAN %s failed: %w", p.PlanKey, err)
			}
			val = result.PrimitiveValue

			span.AddEvent(fmt.Sprintf("  Chain: RUN_PLAN %s", p.PlanKey))

		// Future: case "EMIT_EVENT": ...
		}
	}

	return val, nil
}

// truncate limits a string to maxLen for trace attributes (avoids span bloat).
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
