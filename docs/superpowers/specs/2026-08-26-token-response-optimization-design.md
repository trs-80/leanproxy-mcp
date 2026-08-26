# Token Response Optimization Design

**Date:** 2026-08-26  
**Status:** Approved for implementation  
**Scope:** Backward-compatible Phase 1

## Context

LeanProxy already reduces model context usage through tool discovery, result minification, and character-based response caps. Those behaviors are not applied consistently, however. The MCP handler has optimization and cap logic, while the `serve` command's synchronous, asynchronous, cached, and batch response paths perform their own response handling. Existing caps also apply to individual fields rather than the aggregate response, and runtime token-savings telemetry is not connected to production calls.

This phase creates one shared response-optimization boundary for all tool-call responses. It adds an opt-in, dependency-free estimated-token budget while preserving unlimited behavior by default.

## Goals

- Apply the same response shaping to MCP handler, synchronous, asynchronous, batch, and cache-hit paths.
- Enforce budgets against the complete MCP tool result rather than independently capped fields.
- Add an opt-in estimated-token budget without introducing tokenizer dependencies.
- Preserve existing behavior when no response budget is configured.
- Cache the full losslessly optimized response and apply a caller's budget after cache lookup.
- Record useful before/after optimization telemetry in production.
- Benchmark the real gateway/handler behavior with a large tool catalog.

## Non-goals

- Replacing the router or transport architecture.
- Adding an exact model-specific tokenizer in this phase.
- Making response budgets mandatory or changing the default unlimited response policy.
- Implementing `tools/list` pagination, search indexing, compactor integration, or cached-result summarization. Those remain separate follow-up opportunities.
- Guaranteeing that an estimated-token cap equals a provider's exact tokenizer count.

## Design Decisions

### Shared optimizer

Add a `ResultOptimizer` in `pkg/mcp`. The existing router and transport implementations remain intact; every response path calls the same optimizer before returning an upstream tool result.

The optimizer has two explicit stages:

1. **Prepare** performs lossless normalization: JSON minification and removal of semantically duplicate `structuredContent`.
2. **Limit** resolves the effective opt-in policy and, when necessary, reduces the prepared result to its aggregate budget.

A convenience operation may run both stages for uncached responses. Separating them is required so caches store one full prepared result while different callers can request different budgets.

The logical pipeline is:

```text
upstream response
  -> redact secrets
  -> prepare (lossless minify/deduplicate)
  -> cache full prepared result
  -> resolve caller policy
  -> limit aggregate response when opted in
  -> record before/after telemetry
  -> return response
```

For a cache hit, processing resumes at policy resolution and reapplies the current caller's budget to the cached prepared result.

### Dependency-free token estimation

Define a small token-estimator interface so exact tokenizers can be added later without changing policy or transport code. Phase 1 provides a conservative character-ratio estimator:

```text
estimated tokens = ceil(UTF-8 byte length / chars_per_token)
```

`chars_per_token` defaults to `4.0` and must be positive. The estimate is intentionally documented and surfaced as an estimate, not an exact model token count.

### Policy sources and precedence

Support both the existing character limit and a new estimated-token limit:

- CLI/global: `--max-response-chars`, `--max-response-tokens`
- Per-tool configuration: `max_response_chars`, `max_response_tokens`
- Per-call tool argument: `max_response_chars`, `max_response_tokens`

Precedence for each budget dimension is:

```text
explicit call argument > per-tool configuration > global configuration
```

When both an effective character and token limit exist, enforce whichever produces the tighter aggregate byte allowance. A missing or zero limit means unlimited for that dimension. If neither limit is configured, no lossy limiting occurs.

Proxy-specific per-call arguments are removed before forwarding unless the upstream tool explicitly declares a parameter with the same name. This extends the handler's existing character-cap behavior to token caps.

### Aggregate budget enforcement

Budget measurement covers the serialized result as a whole, including all content items, `structuredContent`, embedded resources, metadata, and inline binary data. This prevents separate fields from each consuming the entire configured cap.

If a prepared response exceeds the effective budget, reductions occur in this order:

1. Retain lossless minification and remove semantically duplicate `structuredContent`.
2. Remove nonessential oversized `_meta` data.
3. Omit distinct oversized `structuredContent` and add an explanatory marker.
4. Truncate text and embedded resource text at line boundaries when practical, then at valid UTF-8 rune boundaries.
5. Replace oversized inline image, audio, or blob data with a textual omission marker.
6. If an over-budget response cannot be parsed, return a minimal valid MCP text result describing the truncation.

The final result must remain valid JSON and valid as an MCP tool result. Markers report estimated original and returned token counts and explain that the caller can request a larger limit. Marker generation must account for its own size so the final serialized payload respects the aggregate allowance whenever the configured allowance is large enough to hold the minimal valid result.

Lossy transformations occur only when the caller has opted into a budget. Unknown result fields are preserved when no lossy limit is active.

### Configuration and validation

Add the following configuration:

- `--max-response-tokens` to relevant server/serve commands.
- `servers[].tools.max_response_tokens` alongside the existing character setting.
- `optimization.chars_per_token`, defaulting to `4.0`.

Positive token limits below the minimum viable response size are rejected during configuration validation rather than silently producing invalid or misleading responses. Invalid, negative, or overflowing values are errors. Existing character settings keep their current accepted syntax and behavior, but their enforcement becomes aggregate.

### Transport integration

The MCP `Handler` owns/configures the shared optimizer and uses it for direct and invoke-style tool calls. The `serve` command uses the same instance or shared API in:

- synchronous upstream responses,
- asynchronous job completion responses,
- individual batch items,
- exact and semantic cache hits.

Transport-specific code supplies context such as server, tool, transport name, arguments, and cache-hit state. It does not implement its own minification or truncation algorithm.

Redaction remains before caching and optimization so secrets cannot be stored or leaked in markers or telemetry.

### Cache semantics

Caches store redacted, losslessly prepared responses without a lossy caller cap. This avoids contaminating later requests with the first caller's smaller budget. On every hit, the effective policy is resolved again and `Limit` runs against the cached prepared response.

Cache keys do not need to include response budgets because budgets are applied after retrieval. Optimization telemetry marks cache hits separately.

### Telemetry

Optimization produces a stats value containing:

- server and tool,
- transport,
- cache-hit state,
- estimated tokens before and after,
- estimated tokens saved,
- whether lossless minification changed the result,
- whether a lossy budget was enforced,
- effective character and estimated-token limits.

Expose the stats through a narrow observer interface. A thread-safe tracker in `pkg/reporter` implements the observer, which prevents `pkg/mcp` from depending directly on reporter internals. A nil observer has negligible behavior impact.

Telemetry must not include response contents, arguments, secrets, or binary payloads.

### Error behavior

- Configuration errors fail at startup with the setting and invalid value identified.
- Upstream JSON that is malformed but under budget passes through as it does today.
- Malformed over-budget results become a minimal valid MCP text result rather than being byte-sliced into invalid JSON.
- Optimizer failures do not panic. With no active lossy budget, the original redacted response is returned. With an active budget, a minimal valid result is returned.

## Compatibility

Unlimited remains the default. Without a configured or explicit response limit:

- no lossy content removal occurs,
- tool argument forwarding remains unchanged except for existing declared proxy-control behavior,
- transport routing and cache selection remain unchanged,
- valid unknown fields are retained.

Lossless JSON minification may change insignificant whitespace where result minification is already enabled. Existing character caps remain supported but become safer aggregate caps.

## Verification Strategy

Implementation follows test-driven development. Required coverage includes:

- estimator rounding and configurable ratio,
- no-policy compatibility,
- explicit/per-tool/global precedence for both dimensions,
- tighter-of character and token limits,
- combined `content` and `structuredContent` aggregate accounting,
- duplicate structured-content removal,
- text and embedded-resource truncation at valid boundaries,
- image/audio/blob omission,
- malformed over-budget fallback,
- cache hits reapplying different caller budgets,
- synchronous, asynchronous, and batch path integration,
- direct and invoke-style handler calls,
- thread-safe telemetry counts and before/after totals,
- configuration parsing and validation.

Token-economy tests and benchmarks must construct the real handler/gateway in lazy and non-lazy modes, including a catalog of approximately 150 tools. Synthetic router-prefix measurements may remain as diagnostics but cannot be the only regression gate.

The complete Go test suite and focused race-sensitive tests for the tracker/optimizer must pass before completion.

## Rollout

The new token cap is opt-in, so the feature can ship without changing existing deployments. Documentation will show character and estimated-token examples, explicitly state the estimation formula, and recommend conservative limits. Operators can compare telemetry before enabling budgets broadly.

Follow-up phases may add exact tokenizers, `tools/list` pagination, indexed tool search, compactor integration, or summary-aware caches behind the interfaces introduced here.
