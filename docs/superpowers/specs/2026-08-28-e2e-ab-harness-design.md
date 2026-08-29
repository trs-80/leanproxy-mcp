# E2E A/B Benchmark Harness — Design

**Date:** 2026-08-28
**Status:** Approved, pending implementation plan
**Supersedes:** nothing. Complements `tests/bench/token_economy_bench_test.go`.

## Problem

The question the project cannot currently answer: **does LeanProxy save tokens on a
real session, and at what MCP schema weight does it start to?**

`make bench` reports 86–98% token savings, but it is an accounting model, not a
measurement. It never starts the proxy — the e2e helper is parked at
`tests/bench/token_economy_bench_test.go:36` (`var _ = buildMockMCP`, "once the
leanproxy e2e harness is in place"). Three modelling choices all bias toward the
proxy:

1. **`token_economy_bench_test.go:391`** charges the proxy arm
   `routerTokens + stubTokens` — the router plus *one* stub, the tool actually
   invoked. A model cannot know which tool it needs before seeing the list; it
   carries the whole manifest every turn. The measured manifest ceiling for this
   setup is ~1,489 tokens. The benchmark charges ~54.
2. **Round trips are absent from the model entirely.** Discovery-then-invoke costs
   an extra assistant turn, which re-sends the whole conversation and burns output
   tokens. That is the proxy's principal cost and it does not appear.
3. **Inputs are synthesised.** Payloads are padded with `.` bytes to hit average
   byte counts from a seeded fixture (`tests/bench/fixtures/live-snapshot.json`),
   so the suite measures an idealised shape rather than real servers.

The session replays (`MorningSport`, `DevWorkflow`, `FullDay`) add no information:
both arms scale linearly in prompt count, so the ratio is fixed and
"FullDay = 95.6%" restates the per-server number.

## Two findings that shape the design

### Finding 1: this is not a two-arm experiment

`cmd/server.go:329` defines `--lazy-tools` as: *"Expose every upstream tool by
prefixed name (`server_tool`) in tools/list with compact stub schemas; clients
call tools directly and the `list_tools`/`invoke_tool`/`search_tools` wrappers are
omitted."*

So there are three distinct configurations, not two:

| Arm | Config | tools/list contents | Discovery round trip |
|---|---|---|---|
| `native` | servers direct in Bob's `mcp.json` | every full schema | none |
| `router` | `server run --stdio` (no `--lazy-tools`) | 3 wrapper tools | **required** |
| `lazy` | `server run --stdio --lazy-tools` | one compact stub per tool | none |

This matters because **`router` is lazy loading; `lazy-tools` is schema
compression**. They have opposite cost structures: `router` minimises residency and
pays round trips, `lazy` pays moderate residency and no round trips.

Bob currently runs the `lazy` arm (`~/.bob/settings/mcp.json`), so Bob pays *no*
discovery round trips today. Any claim about "lazy loading" being the proxy's key
value is, for this deployment, a claim about stub compression instead. The harness
must measure all three arms or it will keep conflating them.

### Finding 2: a config confound must be fixed before measuring

`context7` is enabled in two places simultaneously:

- `~/.bob/settings/mcp.json` — `"context7": {"type":"http", "url":..., "disabled":false}`
- `~/.config/leanproxy_servers.yaml:31` — `enabled: true`

Bob therefore loads context7 tools directly *and* through the proxy. This inflates
baseline schema weight and would confound every arm. The harness must assert a
clean config before it runs, and fail loudly rather than silently measure a
double-loaded setup.

## Architecture

Three layers. Layers 1 and 2 are independent and independently useful; layer 3
only combines their outputs.

### Layer 1 — residency probe (free, deterministic, Go)

**Location:** `tests/bench/e2e/`
**Cost:** zero. No LLM involved.

For each sweep point *k* and each arm, the probe:

1. Starts *k* ballast MCP servers via the existing
   `go build ./tests/bench/mockmcp/cmd` binary with `--tools=N`. `mockmcp.Config`
   already exposes `ToolCount`, `ToolNamePrefix`, `DescriptionBase`, and
   `ResponseBytes`, so schema weight is a precise dial rather than a step function.
2. For `router` and `lazy`: writes a leanproxy config covering ballast plus the two
   real servers, starts `leanproxy-mcp server run --stdio` (with `--lazy-tools` for
   the `lazy` arm), sends `initialize` then `tools/list` over stdio, and captures
   the exact bytes the client receives.
3. For `native`: connects to each server directly and concatenates their
   `tools/list` payloads as a client would hold them.
4. Tokenises every captured payload with `reporter.NewEstimator()`
   (`pkg/reporter/cost.go:82`) — the same primitive the runtime cost tracker uses,
   so these numbers cannot drift from `leanproxy-mcp savings`.

**Output:** exact residency tokens per turn, per arm, per sweep point.

This fixes flaws 1 and 3 directly: the full manifest is charged, and the payloads
are real rather than padded.

A small stdio JSON-RPC client is required and does not exist yet.
`tests/e2e/helper_test.go` provides process lifecycle helpers (`startServe`,
`waitForServeReady`, `runBinaryWithTimeout`) but no `initialize`/`tools/list`
client. It is roughly 60 lines.

### Layer 2 — behavioural probe (coins, Python)

**Location:** `scripts/abbench.py`
**Cost:** ~$6–15 per full run.

Python rather than Go because it drives an external CLI and reads SQLite, and
because `scripts/bobstat.py` already established that shape (stdlib only, Python
3.9+).

Per arm, per selected sweep point:

1. Back up `~/.bob/settings/mcp.json`, write the arm's config, restore on exit
   including on signal or crash. The backup must be restored unconditionally — a
   half-swapped config leaves Bob broken.
2. Run each task in the set via `bob run -p "<prompt>"` as a fresh task.
3. Read the resulting row's `tasks.costs` for ground truth:
   `{input, output, cacheRead, cacheWrite, cost, contextTokens}`. Verified
   populated on 270 of 351 existing tasks.
4. Count `messages` rows with `role='assistant'` for that `task_id` to get turn
   count. (Observed role values: `assistant`, `system`, `tool`, `user`.)
5. Extract `availableTools` from the message JSON — Bob records the exact tool list
   presented on each turn — and assert it matches Layer 1's predicted manifest for
   that arm. The two layers validate each other; a mismatch means one of them is
   wrong and the run is void.
6. Record task success via a per-task assertion: did the expected tool actually get
   called, visible in `messages` rows with `role='tool'`.

**Output:** real tokens, real turn counts, and success/failure per task per arm.
This fixes flaw 2 and adds the discovery-failure signal, which token accounting
cannot see at all.

### Layer 3 — combination and reporting

```
net_tokens_per_task(k, arm) = residency(k, arm) × turns(k, arm) + output(k, arm)
```

Layer 1 supplies `residency` exactly at every *k*. Layer 2 supplies `turns` and
`output` only at the points actually run. Points are labelled `measured` or
`derived` in the output; the harness never interpolates silently.

## Parameters

- **Ballast sweep:** 0 / 25 / 50 / 100 / 200 tools, on top of the ~10 real tools
  (codebase-memory contributes 8 after its `include` filter; context7 contributes 2).
  Layer 1 runs all five points across all three arms.
- **Live points:** k=0 and k=100, all three arms — the bracket most likely to
  straddle breakeven.
- **Task set:** 5 fixed prompts drawn from real recorded history in the `messages`
  table. The specific prompts are chosen during implementation against three
  criteria: each must exercise at least one real server so round trips are genuine,
  each must have a deterministic success assertion (a named tool that must appear in
  the task's `role='tool'` rows), and the set must span the observed cost range
  rather than clustering at one difficulty. Once chosen they are frozen in a fixture
  file so runs remain comparable across dates.
- **Cost:** 2 points × 3 arms × 5 tasks = 30 runs, ≈ $9–20 at observed per-task
  costs of $0.10–$1.30.

## Statistical treatment

Observed per-task cost spans an order of magnitude ($0.10–$1.30). With one
repetition per cell, task difficulty will swamp a modest proxy effect if results
are aggregated as means.

The harness therefore reports **paired per-task deltas** as the primary statistic:
same prompt, different arms, subtracted. Pairing cancels most task-difficulty
variance. Aggregate means are also reported but explicitly flagged as the weaker
statistic. Where the paired delta's sign is inconsistent across tasks, the harness
reports "no detectable effect" rather than a point estimate.

## Outputs

- `bench-results/e2e-<timestamp>.json` — machine-readable, one record per
  (arm, sweep point, task).
- Terminal table, matching `bobstat.py`'s existing presentation conventions.
- `make bench-e2e` target.

## Testing and safety

- **Layer 1** is an ordinary Go test: deterministic, mockmcp-only, no real servers,
  no network, no coins. Safe in CI.
- **Layer 2** is opt-in behind an environment variable so CI can never spend coins.
  It refuses to run if the config confound in Finding 2 is present, and it restores
  `~/.bob/settings/mcp.json` unconditionally on exit.
- Neither layer writes to `~/.config/leanproxy_servers.yaml`; arm configs are
  written to temporary directories and passed via `--config`.

## Expected result

Layer 1 alone will likely answer the original question for free. At ~10 real tools,
the residency saving is small in absolute terms while the `router` arm's floor plus
round trips is not, so the proxy is expected to sit near or below breakeven on the
current setup. The prior measurement that ~70% of Bob's tool payload is Bob's own
built-in `read_file` — which the proxy cannot touch — shrinks the addressable share
further.

Stated plainly: a 96% saving on the MCP schema slice of a payload where MCP is a
small minority is a small saving on the bill. The harness exists to put a number on
that rather than argue about it.

## Out of scope

- Rotating the context7 API key currently in plaintext at
  `~/.config/leanproxy_servers.yaml:38`. Flagged, tracked separately.
- Changing the existing `token_economy_bench_test.go` suite. Once the harness
  produces real numbers, the modelled suite's thresholds should be revisited, but
  that is follow-on work.
- Measuring latency. Per project convention, this work reports tokens and cost per
  task, not nanoseconds per operation.
