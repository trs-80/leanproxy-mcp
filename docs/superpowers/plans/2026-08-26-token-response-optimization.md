# Token Response Optimization Implementation Plan

> **Execution requirement:** Follow this plan with the test-driven-development and verification-before-completion workflows. Write each failing test before its implementation and keep commits limited to the task being completed.

**Goal:** Apply one dependency-free, opt-in aggregate response-budget pipeline to every MCP tool-result path while preserving unlimited behavior and recording estimated token savings.

**Architecture:** A neutral `pkg/tokenbudget` package owns the estimator contract, policy/stats values, and observer contract. `pkg/mcp.ResultOptimizer` performs lossless preparation and aggregate limiting. `mcp.Handler` owns policy resolution and exposes the operations needed by `cmd/serve.go`; caches store prepared results and callers are limited after lookup. `pkg/reporter` implements the observer contract without creating an import cycle.

**Tech stack:** Go standard library, existing MCP/proxy types, Cobra configuration, Go tests/benchmarks, race detector.

**Design reference:** `docs/superpowers/specs/2026-08-26-token-response-optimization-design.md`

---

## Task 1: Add the dependency-free estimator and optimization contracts

**Files:**

- Create: `pkg/tokenbudget/estimator.go`
- Create: `pkg/tokenbudget/estimator_test.go`
- Create: `pkg/tokenbudget/stats.go`
- Modify: `pkg/reporter/cost.go`
- Modify: `pkg/reporter/cost_test.go`

### Step 1: Write failing estimator tests

Cover an interface-backed character-ratio estimator with:

- default `4.0` characters per estimated token,
- ceiling behavior (`1..4` bytes is one token, `5` bytes is two),
- UTF-8 measured by serialized byte length,
- rejection of zero, negative, NaN, and infinite ratios,
- conversion of a token cap to a conservative byte allowance.

Add contract tests for `Policy`, `Stats`, and the small `Observer` interface. Use a minimum estimated-token limit of 50, matching the existing 200-character viability floor at the default ratio.

Run:

```bash
go test ./pkg/tokenbudget
```

Expected: FAIL because the package does not exist.

### Step 2: Implement the contracts

Implement types equivalent to:

```go
type Estimator interface {
    Estimate([]byte) int
    ByteAllowance(tokens int) int
}

type CharRatioEstimator struct { /* validated ratio */ }

type Policy struct {
    MaxChars  int
    MaxTokens int
    ExplicitChars  bool
    ExplicitTokens bool
}

type Stats struct {
    Server, Tool, Transport string
    CacheHit, Minified, BudgetEnforced bool
    TokensBefore, TokensAfter, TokensSaved int
    EffectiveMaxChars, EffectiveMaxTokens int
}

type Observer interface { ObserveOptimization(Stats) }
```

Keep construction explicit so invalid ratios return an error. Do not add an external tokenizer.

### Step 3: Make the existing reporter estimator delegate to the shared estimator

Preserve the public `reporter.NewEstimator()` and its current results while delegating token estimation to `pkg/tokenbudget`. This prevents production telemetry and regression tests from drifting apart.

Run:

```bash
go test ./pkg/tokenbudget ./pkg/reporter
```

Expected: PASS.

### Step 4: Commit

```bash
git add pkg/tokenbudget pkg/reporter/cost.go pkg/reporter/cost_test.go
git commit -m "feat: add shared token budget estimator"
```

## Task 2: Build the lossless prepare and aggregate limit engine

**Files:**

- Create: `pkg/mcp/result_optimizer.go`
- Create: `pkg/mcp/result_optimizer_test.go`
- Modify: `pkg/mcp/minify.go`
- Modify: `pkg/mcp/minify_test.go`
- Modify: `pkg/mcp/caps.go`
- Modify: `pkg/mcp/caps_test.go`
- Modify: `pkg/mcp/token_reduction_lazy_test.go`

### Step 1: Write failing preparation tests

Pin the current lossless behavior behind `ResultOptimizer.Prepare`:

- compact JSON embedded in text,
- remove only byte-equivalent `structuredContent`,
- retain distinct structured content and unknown envelope fields,
- preserve malformed input,
- return stats indicating whether the result changed.

Run:

```bash
go test ./pkg/mcp -run 'TestResultOptimizerPrepare'
```

Expected: FAIL because `ResultOptimizer` is undefined.

### Step 2: Implement `Prepare`

Move/reuse the semantics in `minifyToolResult` without duplicating its algorithm. Keep `minifyToolResult` as a compatibility wrapper while callers migrate.

### Step 3: Write failing aggregate budget tests

Test `Limit` and `Optimize` for:

- combined serialized `content` plus `structuredContent` accounting,
- tighter-of character and estimated-token limits,
- no-policy pass-through,
- `_meta` removal before user-visible text loss,
- distinct oversized `structuredContent` omission,
- line-boundary then rune-boundary text truncation,
- embedded resource text truncation,
- image/audio/blob replacement with a marker,
- marker self-accounting so output stays under the allowance,
- valid minimal MCP result for malformed over-budget input,
- preservation of `isError` and unknown top-level fields when possible,
- deterministic output and no panic for unfamiliar content blocks.

Run:

```bash
go test ./pkg/mcp -run 'TestResultOptimizerLimit|TestResultOptimizerMalformed'
```

Expected: FAIL.

### Step 4: Implement aggregate limiting

Parse envelope and content fields as `json.RawMessage` to preserve numeric precision and unknown data. Apply the ordered reduction algorithm from the design. Measure the fully serialized result after every reduction, and reserve space for a marker before retaining text. Always return valid UTF-8 and JSON.

Turn `truncateToolResult` into a compatibility wrapper over the new limiter, then update its tests to assert the aggregate guarantee rather than field-local character totals.

Run:

```bash
go test ./pkg/mcp
```

Expected: PASS.

### Step 5: Commit

```bash
git add pkg/mcp/result_optimizer.go pkg/mcp/result_optimizer_test.go pkg/mcp/minify.go pkg/mcp/minify_test.go pkg/mcp/caps.go pkg/mcp/caps_test.go pkg/mcp/token_reduction_lazy_test.go
git commit -m "feat: enforce aggregate tool result budgets"
```

## Task 3: Resolve policies in `mcp.Handler` and preserve cache semantics

**Files:**

- Modify: `pkg/mcp/handlers.go`
- Modify: `pkg/mcp/caps.go`
- Modify: `pkg/mcp/toolcall.go`
- Modify: `pkg/mcp/resultcache.go`
- Modify: `pkg/mcp/caps_test.go`
- Modify: `pkg/mcp/resultcache_test.go`
- Modify: `pkg/mcp/token_reduction_test.go`
- Modify: `pkg/mcp/token_reduction_lazy_test.go`

### Step 1: Write failing policy tests

Cover independent precedence for character and token dimensions:

```text
explicit call value > server/tool value > global value
```

Also cover:

- numeric and numeric-string explicit values,
- explicit token values clamped to the 50-token viability floor,
- configured values below the floor rejected by configuration wiring later,
- stripping both proxy arguments before upstream forwarding,
- retaining either argument when the upstream schema declares it,
- exact preservation of 64-bit sibling argument values.

Run:

```bash
go test ./pkg/mcp -run 'TestResponseBudget|TestExtractResponseBudget'
```

Expected: FAIL.

### Step 2: Add handler configuration and exported response operations

Extend `Handler` with:

- default/per-tool token-cap state,
- a configured `ResultOptimizer`,
- setters for character ratio, observer, defaults, and per-tool token limits,
- one policy resolver,
- an extraction method returning cleaned arguments plus explicit policy,
- exported prepare/limit helpers used by `cmd/serve.go`.

Keep the current setter initialization rule: configure before serving, so hot-path maps need no new locks.

### Step 3: Write failing direct/invoke/cache tests

Prove both direct `tools/call` and `invoke_tool`:

- prepare before storing an exact result,
- store the full prepared result,
- apply the effective budget after a miss or hit,
- allow two callers with different explicit budgets to share one cache entry and receive differently sized valid responses,
- emit observer stats with correct cache-hit and transport context.

Run:

```bash
go test ./pkg/mcp -run 'Test.*ResponseBudget|TestResultCache.*Budget|TestInvokeTool.*Token'
```

Expected: FAIL, then PASS after integration.

### Step 4: Replace duplicated handler call-site logic

In both paths in `pkg/mcp/toolcall.go`, use:

```text
extract explicit policy -> cache lookup -> prepare on miss -> cache prepared result -> limit -> observe
```

Remove the separate minify/cap sequences once both paths use the shared optimizer.

Run:

```bash
go test ./pkg/mcp
```

Expected: PASS.

### Step 5: Commit

```bash
git add pkg/mcp/handlers.go pkg/mcp/caps.go pkg/mcp/toolcall.go pkg/mcp/resultcache.go pkg/mcp/*_test.go
git commit -m "feat: apply response budgets through mcp handler"
```

## Task 4: Add production optimization telemetry

**Files:**

- Create: `pkg/reporter/optimization.go`
- Create: `pkg/reporter/optimization_test.go`
- Modify: `cmd/server.go`
- Modify: `cmd/serve.go`

### Step 1: Write failing tracker tests

Test a thread-safe tracker that:

- implements `tokenbudget.Observer`,
- counts calls, cache hits, minified calls, and budgeted calls,
- aggregates estimated before/after/saved tokens,
- groups by server/tool/transport without storing response content,
- returns defensive snapshots,
- remains race-free under concurrent observations.

Run:

```bash
go test ./pkg/reporter -run TestOptimizationTracker
go test -race ./pkg/reporter -run TestOptimizationTrackerConcurrent
```

Expected: FAIL.

### Step 2: Implement and wire the tracker

Create a global/default tracker consistently with the existing cost tracker. Inject it into every `mcp.Handler` built by `cmd/server.go` and `cmd/serve.go`. Do not log raw results or arguments.

Run:

```bash
go test ./pkg/reporter ./cmd
```

Expected: PASS.

### Step 3: Commit

```bash
git add pkg/reporter/optimization.go pkg/reporter/optimization_test.go cmd/server.go cmd/serve.go
git commit -m "feat: track response token optimization"
```

## Task 5: Add configuration, CLI flags, and validation

**Files:**

- Modify: `pkg/migrate/config.go`
- Modify: `pkg/migrate/config_test.go` or the existing adjacent configuration tests
- Modify: `cmd/server.go`
- Modify: `cmd/serve.go`
- Modify: relevant `cmd/*_test.go` flag/config tests

### Step 1: Write failing configuration tests

Cover:

- `optimization.chars_per_token` default `4.0`,
- global `max_response_tokens`,
- `servers[].tools.max_response_tokens`,
- CLI `--max-response-tokens`,
- zero as unlimited,
- startup errors for negative, overflowing, or positive values below 50,
- invalid character ratios,
- existing character-only configurations remaining valid.

Run:

```bash
go test ./pkg/migrate ./cmd -run 'Test.*ResponseToken|Test.*CharsPerToken'
```

Expected: FAIL.

### Step 2: Extend config structs and command wiring

Add JSON/YAML tags beside the existing character settings. Bind flags in both server entry points and apply settings to the shared handler/optimizer in the same functions that currently apply `max_response_chars` and minification.

Prefer returning descriptive validation errors over silently coercing invalid configured values. Keep zero/unset unlimited.

Run:

```bash
go test ./pkg/migrate ./cmd
```

Expected: PASS.

### Step 3: Commit

```bash
git add pkg/migrate cmd/server.go cmd/serve.go cmd/*_test.go
git commit -m "feat: configure estimated token response caps"
```

## Task 6: Route every `serve` response through the shared optimizer

**Files:**

- Modify: `cmd/serve.go`
- Modify: `cmd/serve_test.go`
- Modify: `cmd/serve_redaction_test.go`
- Modify: `cmd/serve_upstream_error_test.go`
- Add or modify: focused serve response-budget tests under `cmd/`

### Step 1: Introduce failing transport parity tests

Build a large tool result containing text, distinct structured content, metadata, and inline binary content. Assert identical valid limited results and stats for:

- synchronous single requests,
- asynchronous single requests,
- synchronous batch entries,
- asynchronous batch entries,
- semantic cache hits.

Also assert no lossy changes when no budget is configured and ensure redaction occurs before preparation/caching.

Run:

```bash
go test ./cmd -run 'TestServeResponseBudget|TestHandleBatch.*ResponseBudget|TestSemanticCache.*ResponseBudget'
```

Expected: FAIL.

### Step 2: Add one serve-side response helper

Create a small helper/context in `cmd/serve.go` that:

- recognizes successful tool-call responses,
- extracts and removes explicit proxy budget arguments while preserving upstream-declared parameters,
- prepares a redacted miss before semantic cache storage,
- limits both misses and cache hits using `mcp.Handler`,
- observes server/tool/transport/cache-hit metadata,
- leaves JSON-RPC errors and non-tool methods unchanged.

Pass this helper through the existing connection/batch functions rather than adding global mutable policy state.

### Step 3: Integrate all response paths

Update the pipelines around `handleConnection`, `handleBatchRequest`, `handleBatchRequestAsync`, and `processBatchItem`. Ensure `semanticCacheStore` receives the prepared full result and `cachedResponse` is limited only after retrieval.

Preserve request ordering, notification behavior, timeouts, provider accounting, injection checks, and redaction failures.

Run:

```bash
go test ./cmd
go test -race ./cmd -run 'TestHandleBatchRequestAsync|TestServeResponseBudget'
```

Expected: PASS.

### Step 4: Commit

```bash
git add cmd/serve.go cmd/*serve*_test.go
git commit -m "feat: optimize responses across serve transports"
```

## Task 7: Replace synthetic-only token gates with real handler coverage

**Files:**

- Modify: `pkg/mcp/token_economy_guard_test.go`
- Modify: `tests/bench/token_economy_bench_test.go`
- Add fixtures/helpers only if needed under `tests/bench/`

### Step 1: Write real-handler regression tests

Construct approximately 150 cached tools and measure actual `Handler` output for:

- non-lazy `tools/list`,
- lazy `tools/list`,
- gateway-only fixed prefix,
- prepared and capped large results.

Set explicit budgets based on measured current behavior plus documented headroom. Do not use the hand-built three-tool router JSON as the sole assertion.

Run:

```bash
go test -v ./pkg/mcp -run TestTokenEconomy
go test -v ./tests/bench -run TestTokenEconomy
```

Expected: the new test initially exposes the difference between the synthetic router metric and actual handler output.

### Step 2: Update benchmarks

Add benchmark subcases for real handler construction and response generation in lazy/non-lazy modes. Retain synthetic measurements only as separately labeled diagnostics.

Run:

```bash
go test -run '^$' -bench 'Token|Schema|Gateway' -benchmem ./tests/bench ./pkg/mcp
```

Expected: PASS with real-handler token metrics printed.

### Step 3: Commit

```bash
git add pkg/mcp/token_economy_guard_test.go tests/bench
git commit -m "test: benchmark real handler token economy"
```

## Task 8: Document and fully verify the feature

**Files:**

- Modify: `docs/configuration.md`
- Modify: `docs/budget.md`
- Modify: `docs/commands.md`
- Modify: `docs/benchmark-results.md` only if generated measurements are intentionally recorded

### Step 1: Update user documentation

Document:

- opt-in/default-unlimited behavior,
- CLI, global, per-tool, and explicit-call examples,
- precedence and tighter-of semantics,
- `ceil(bytes / chars_per_token)` estimation formula and default ratio,
- aggregate response accounting and omission markers,
- cache behavior,
- optimization telemetry fields,
- exact-tokenizer limitation.

### Step 2: Run focused and full verification

```bash
gofmt -w pkg/tokenbudget/*.go pkg/mcp/*.go pkg/reporter/*.go pkg/migrate/*.go cmd/*.go
go test ./pkg/tokenbudget ./pkg/mcp ./pkg/reporter ./pkg/migrate ./cmd ./tests/bench
go test -race ./pkg/tokenbudget ./pkg/mcp ./pkg/reporter ./cmd
go test ./...
git diff --check
```

If a sandbox blocks local test listeners, rerun the same Go test command with the required local-bind permission; do not weaken tests.

### Step 3: Refresh the code graph and inspect the final diff

```bash
graphify update .
git status --short
git diff --stat <pre-implementation-base>..HEAD
```

Use codebase-memory impact/coverage checks for every changed production path and read any reported missed ranges directly before claiming completion.

### Step 4: Request code review

Use the requesting-code-review workflow against the approved design and this plan. Address confirmed issues with new failing tests first.

### Step 5: Commit documentation and final adjustments

```bash
git add docs pkg cmd tests
git commit -m "docs: explain aggregate response token budgets"
```

The final handoff must report the exact verification commands and results, configuration compatibility, measured real-handler token figures, and any remaining follow-up work.
