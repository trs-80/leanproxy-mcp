# Benchmark Results

This document is the **single source of truth** for the token-economy and
NFR performance numbers in the [README](https://github.com/mmornati/leanproxy-mcp/blob/main/README.md) and
[index.md](./index.md). Every number below is produced by an executable
test in `tests/bench/token_economy_bench_test.go` and re-validated by
`make bench`. No number here is hand-edited.

> **TL;DR** — All headline claims from the README pass. Measured savings are
> **86-99%** (depending on server shape), proxy overhead is **~12 µs/op**
> (NFR1 wants <50 ms), and throughput is **~25,000 q/s in-process** (NFR
> AC 16-3 wants ≥500 q/s). One previously-claimed number is corrected:
> the **router payload is 158 tokens**, not the older ~110 / 27.5 — see
> [§3 Why the router number moved](#3-why-the-router-number-moved).

## 1. How to reproduce

```bash
# from repo root
make bench              # runs the full suite, writes bench-results/<date>.txt
make bench-compare      # diffs two result files (FILES=old.txt new.txt)
go test -run=^$ -bench=. -benchmem -benchtime=3s -count=3 ./tests/bench/...
```

The full suite runs in **< 1 second** end-to-end on a single core. It
does **not** require a live MCP server, network access, or a database —
every server-side path is exercised against the in-process
`tests/bench/mockmcp` library.

To refresh the canonical MCP server tool counts from a live run (e.g.
after re-installing the GitHub / Garmin / Intervals.icu servers):

```bash
go run ./tests/bench/live_snapshot \
    -config tests/bench/fixtures/live-snapshot.yaml \
    -out   tests/bench/fixtures/live-snapshot.json
```

Until that snapshot is refreshed, `tests/bench/fixtures/live-snapshot.json`
contains seeded counts derived from the previous `docs/index.md` numbers
(GitHub 41, Garmin 100, Intervals.icu 10 — Stitch is no longer available).

## 2. Methodology

### 2.1 Token counting

All token accounting in the benchmark suite uses the **same primitive**
the runtime cost tracker uses:

```go
import "github.com/mmornati/leanproxy-mcp/pkg/reporter"

estimator := reporter.NewEstimator()        // 1 token ≈ 4 chars (chars/4)
tokens := estimator.EstimateTokens(payload)  // or EstimateJSON(v)
```

This is `pkg/reporter.Estimator`, exposed as a public type in v0.9.0
specifically so the runtime and benchmarks can never disagree. The
`chars/4` heuristic is a well-known approximation of BPE-style
tokenizers (OpenAI, Anthropic) for English text; it is not byte-perfect
but is **consistent within itself**, which is what matters for the
savings ratios reported here.

### 2.2 Native MCP baseline

For each MCP server in the live snapshot, the benchmark synthesises a
`tools/list` JSON payload whose byte size matches the per-server
`schema_bytes` field. The token count of that payload is the "Native MCP"
column. We use the **raw** token count (not the 0.25× cache-read cost)
because the README's "Schema Tax" claim is about the on-wire payload
size, independent of provider-side caching discounts.

The cache-read comparison in `index.md` is provided as a separate
column at 0.25× to mirror the original table.

### 2.3 LeanProxy router payload

The router is a 3-tool definition exposed by `pkg/gateway/tools.go`:

- `list_servers` — list MCP servers configured
- `invoke_tool` — invoke a tool on a specific server
- `list_tools` — list tools on a specific server

The benchmark marshals this into the same `{"jsonrpc":"2.0","id":1,
"result":{"tools":[...]}}` envelope the production proxy returns, so
the 158-token figure includes the JSON-RPC envelope (id, jsonrpc
version, result wrapper).

### 2.4 Session replays

The three session tables (Morning Sport / Dev / Full Day) replay the
exact prompt sequences from the previous `docs/index.md` against the
synthesised per-server tool counts:

- **Morning Sport** — 2 servers, 4 prompts (Garmin + Intervals.icu)
- **Dev Workflow** — 2 servers, 5 prompts (GitHub + Intervals.icu)
- **Full Day** — 3 servers, 7 prompts (all available)

For each prompt, the "Native MCP" cost is the sum of the per-server
schema tax **at 0.25× cache-read cost** (matching the "real" cost model
in `index.md`). The "LeanProxy" cost is the router payload + one stub
schema (~26 tokens) per tool actually invoked.

### 2.5 NFR benchmarks

- **NFR1 (proxy overhead)** — microbenchmark of the JSON-RPC parse +
  cost-track hot path. Includes one Unmarshal of the request, one
  Unmarshal of the response, and one `TrackAt` call.
- **NFR2 (50 MB payload)** — single-call `EstimateTokens` on a 50 MB
  byte buffer. This is the worst-case size a single request can hit.
- **AC 16-3 (throughput)** — in-process `mockmcp.Server` driven in a
  tight loop. The number is the **mockmcp** ceiling, not the
  leanproxy binary ceiling; for a real binary-level measurement, use
  the in-tree e2e suite (`tests/e2e/`) which currently exercises
  ~5,000 req/s against a Python mock upstream.
- **NFR3 (binary size)** — `os.Stat` over the `dist/leanproxy-mcp-*`
  binaries produced by `make build`.

## 3. Why the router number moved

The README's previous "~110 router tokens" / "27 tokens" came from a
hand-counted estimate of the 3-tool schema (without the JSON-RPC
envelope) using a different token-counting rule (1 token per tool
field, then summed). The benchmark measures the **full `tools/list`
response as it would appear on the wire** using the runtime Estimator,
which is the right unit for the cost-saving claim.

For comparison:

| Measurement | Tokens | Source |
|---|---|---|
| Hand-counted 3-tool field sum (old) | ~27 | previous `docs/index.md` |
| Hand-counted 3-tool schema (old) | ~110 | previous README |
| Full `tools/list` envelope (current) | **158** | `tests/bench` + Estimator |
| Per-stub on-demand schema (current) | **26** | `tests/bench` + `registry.ToolStub` |

## 4. Raw results (latest run, v0.9.0)

```
goos: darwin
goarch: arm64
pkg: github.com/mmornati/leanproxy-mcp/tests/bench
cpu: Apple M4
```

### 4.1 Schema-tax (per server)

| Server | Tools | Native tokens | Router tokens | Savings |
|---|---:|---:|---:|---:|
| Garmin | 100 | 11,134 | 158 | **98.6%** |
| GitHub | 41 | 4,570 | 158 | **96.5%** |
| Intervals.icu | 10 | 1,129 | 158 | **86.0%** |
| All 3 | 151 | 16,833 | 158 | **99.1%** |

### 4.2 Session replays (0.25× cache-read model)

| Session | Prompts | Native tokens | Lean tokens | Savings |
|---|---:|---:|---:|---:|
| Morning Sport | 4 | 12,260 | 740 | **94.0%** |
| Dev Workflow | 5 | 7,120 | 925 | **87.0%** |
| Full Day | 7 | 29,449 | 1,295 | **95.6%** |

### 4.3 NFRs

| Benchmark | Measured | Threshold | Pass |
|---|---|---|:-:|
| `BenchmarkProxyOverhead_NFR1` (p50) | ~12 µs/op | <50 ms | ✅ |
| `BenchmarkLargePayload_NFR2` (50 MB) | ~7 ms | <200 ms | ✅ |
| `BenchmarkThroughput_MockMCP` (in-process) | ~25,000 q/s | ≥500 q/s | ✅ |
| `TestBinarySize_NFR3` (darwin-arm64) | 15.8 MB | <20 MB | ✅ |

### 4.4 Per-primitive microbenchmarks

| Primitive | Time | Allocs |
|---|---:|---:|
| `BenchmarkEstimateTokens` | ~50 ns | 0 |
| `BenchmarkEstimateJSON` | ~250 ns | 1 |

## 5. What was corrected vs the previous README

| Old claim | Source | New claim | Notes |
|---|---|---|---|
| "90%+" headline | README | "86-99%" | Per-server variation is real |
| "~110 router tokens" | README, architecture | **158 tokens** | Now includes the JSON-RPC envelope (full `tools/list`) |
| "27.5 LeanProxy tokens" | index.md | **158 tokens** | Same correction; old number was a hand-counted schema-field sum, not the on-wire payload |
| "~54 tokens per stub" | configuration.md, architecture | **~26 tokens per stub** | Stub is `{name, description, category?}` — measured from the production `registry.ToolStub` |
| "11 µs overhead at 5,000 RPS" | architecture.md | **~12 µs/op (p50)** | Same order of magnitude; quoted from Bifrost originally — now our own number |
| "6-7× token reduction" | configuration.md, architecture | **86-99% reduction** | Per-server ratio varies; use the new tables |
| 4-server column | README, index.md | **3-server (Stitch removed)** | Stitch MCP is no longer available |
| Garmin 55 / Intervals 67 (README) | README | **Garmin 100 / Intervals 10** | Resolved against `docs/index.md`; now consistent across both docs |

## 6. Future work

- **Refresh `live-snapshot.json`** with a real run of `go run
  ./tests/bench/live_snapshot` once the Garmin / Intervals / GitHub
  credentials are wired into CI. The seeded numbers in
  `fixtures/live-snapshot.json` are a placeholder.
- **Binary-level throughput** — the in-process mockmcp number is a
  ceiling, not the binary-level throughput. A `make bench-e2e` target
  that spawns the leanproxy binary + a mockmcp subprocess and measures
  end-to-end q/s is the next step (see `buildMockMCP` in
  `token_economy_bench_test.go`).
- **Real-tokenizer comparison** — swap the `chars/4` heuristic for
  `tiktoken-go` to get a tokenizer-accurate number. The Estimator API
  already supports this via `NewEstimatorWithRatio`.

## 7. Commit / run metadata

| Field | Value |
|---|---|
| Version | v0.9.0 |
| Git | `7db8011` (Merge pull request #269) |
| Go | 1.25+ |
| Host | Apple M4, darwin/arm64 |
| Date | 2026-08-12 |

## 8. Modelled numbers vs. measured numbers

The figures at the top of this document and in the README come from
`tests/bench/token_economy_bench_test.go`, which is a **schema-tax accounting
model**, not an end-to-end measurement. It never starts the proxy. Three of its
modelling choices bias toward LeanProxy:

1. It charges the proxy arm for the router plus *one* tool stub — the tool
   actually invoked. A model cannot know which tool it needs before seeing the
   list, so in practice it carries the whole manifest every turn.
2. Discovery round trips are absent from the model entirely. They are the
   proxy's principal cost.
3. Payloads are synthesised and padded to hit average byte counts, rather than
   captured from real servers.

Those numbers are correct for what they measure: the size of the tool-schema
slice. They are not a claim about session cost.

## 9. The e2e harness

`tests/bench/e2e` measures instead of modelling. It starts the real proxy over
stdio and captures the exact `tools/list` bytes a client receives.

### 9.1 Three arms, not two

| Arm | Config | tools/list contents | Discovery round trip |
|---|---|---|---|
| `native` | servers configured directly in the client | every full schema | none |
| `router` | `server run --stdio` | 3 wrapper tools | **required** |
| `lazy` | `server run --stdio --lazy-tools` | one compact stub per tool | none |

`router` is lazy loading: minimal residency, paid for with an extra turn.
`lazy` is schema compression: moderate residency, no extra turn. They have
opposite cost structures and must not be conflated.

### 9.2 Running it

```bash
make bench-e2e        # Layer 1: residency sweep across all arms. No LLM, no coins.

# Layer 2: live A/B through real sessions. SPENDS COINS. The target refuses
# to run unless the caller supplies LEANPROXY_AB_LIVE=1 itself — that is not
# a default the Makefile sets for you:
LEANPROXY_AB_LIVE=1 make bench-e2e-live

# Layer 3: join Layer 1 + Layer 2 into a net-tokens-per-task table:
python3 scripts/abreport.py bench-results/e2e-residency-*.json \
                            bench-results/e2e-live-*.json
```

**Layer 2 runs an unsupervised, write-capable agent in `--cwd` (default: this
repo).** The default sweep is 3 arms × 5 tasks × 2 ballast points = 30
autonomous sessions. The fixture prompts are read-only questions, but nothing
constrains the agent to them, so `abbench.py` refuses to start on a dirty
working tree (`--allow-dirty-repo` overrides) and reports anything that changed
once the sweep finishes.

**Layer 2 refuses before it spends anything.** Ahead of the first `bob run` it
checks that no server is loaded both directly and through the proxy (by name
*and* by resolved command path or URL, so the same upstream under two different
names is still caught), that every proxied server can be attached directly for
the native arm, and — by starting each arm's servers and asking them for their
tools — that all three arms can actually reach every tool
`tests/bench/e2e/fixtures/tasks.json` expects. A configuration that would
produce no verdict is a refusal with an empty bill, not a discovery made after
the budget is gone. `--skip-preflight` bypasses this and is documented as
spending real money on an unverified configuration.

**The arms mirror Layer 1's topology exactly**, because Layer 3 joins the two
layers on `ballast_tools` alone and multiplies one layer's turn count by the
other's residency figure:

| Arm | Proxied servers | Ballast |
|---|---|---|
| `native` | attached directly to the agent | attached directly to the agent |
| `router` | behind the proxy | behind the proxy |
| `lazy` | behind the proxy | behind the proxy |

Both layers take the ballast tool description from the single fixture
`tests/bench/e2e/fixtures/ballast.json` (Layer 1 embeds it with `//go:embed`;
Layer 2 reads it), and `TestBallastWeightIsIdenticalAcrossLayers` fails on a
one-byte difference between the two layers' real `tools/list` payloads.

The proxy arms run against a generated copy of the operator's
`leanproxy_servers.yaml` with the ballast spliced in and `adaptive_stub_after`
stripped — under adaptive stubs the proxy's `tools/list` depends on usage
recorded in `~/.config/leanproxy/toolusage.json`, which every live run mutates,
so the lazy arm's residency would drift mid-sweep and stop describing the fixed
figure Layer 1 measured.

`make bench-e2e` sweeps ballast tool counts `{2, 4, 8, 25, 50, 100, 200}`
(`ballastPoints` in `tests/bench/e2e/residency_test.go`), split across 2
synthetic servers. Integer division across those servers means the count
actually created can come in one short of the nominal point — e.g. 25 tools
over 2 servers yields 24, not 25 — and the harness reports the actual count
it built, not the nominal sweep point, which is why `bench-results/e2e-
residency-*.json` shows `ballast_tools: 24` rather than `25`. The low end of
the sweep (2, 4, 8) is not padding: it exists specifically to catch two
different crossovers in that range (see §9.3).

### 9.3 Reading the residency numbers (measured, `bench-results/e2e-residency-*.json`)

These are Layer 1 only — no live model was run to produce them:

- `router` is **flat**: 2174 bytes / 544 estimated tokens at every ballast
  level from 2 to 200 tools. Its wrapper schema (3 tools) does not grow with
  the catalogue size.
- `native` costs **~676-677 bytes/tool** at every measured level (677 B/tool
  at 100 and 200 tools).
- `lazy` costs **~276-277 bytes/tool** at every measured level (277 B/tool at
  100 and 200 tools).
- `lazy/native` is flat at **~0.409** across the sweep (e.g. 27,691 / 67,705
  bytes at 100 tools).
- `router`'s fixed floor exceeds `native` only below ~4 tools: at 2 tools
  native is 1377 B vs. router's 2174 B; by 4 tools native (2729 B) has
  already overtaken router.
- The full three-way ordering `router < lazy < native` first holds at 8
  ballast tools (2174 < 2219 < 5433 B); at 4 tools it does not (`lazy` at
  1115 B undercuts `router`'s floor).

### 9.4 `scripts/abreport.py` — Layer 3

`abreport.py` joins the Layer 1 residency sweep with whatever Layer 2 live
data exists into `net_tokens = residency_tokens × turns + output_tokens` per
task, and prints the breakeven ballast level for each proxy arm: the point
past which it costs fewer tokens per task than `native`. That line is the
answer this harness was built to produce — but it only prints where live data
exists to back it, and it enforces several honesty rules rather than
smoothing over gaps in the data:

- Every row is labelled **`measured`** (a live run exists at that exact
  ballast level) or **`derived`** (the turn count is borrowed from the
  nearest ballast level that does have live data). The label is a column in
  the table and a tag on every verdict line, never hidden.
- A live run that completed without finding the expected tool
  (`succeeded: False`) is excluded from turn/output averages — including it
  would let a badly-failing arm look cheapest, since a model that gives up
  early posts a low turn count. The success rate that exclusion implies is
  printed next to every verdict, and below `MIN_SUCCESS_RATE_FOR_VERDICT`
  (0.5) no saves/costs-more number is printed at all for that arm/level.
- A `derived` verdict is stress-tested against a ±1 turn error in the
  borrowed turn count and tagged `FLIPS` if that single-turn perturbation
  changes the verdict's sign, `stable` otherwise.
- Comparisons are paired by task id wherever both arms share one — an
  unpaired mean over a handful of tasks whose per-task cost spans an order of
  magnitude reports task difficulty, not arm effect. Fewer than 2 paired (or,
  falling back to unpaired, per-arm) samples and no verdict is printed —
  a single pair cannot disagree with itself.
- An arm with no usable live data anywhere is omitted from the table, and the
  omission is named and explained rather than left for the reader to notice.
- The report checks itself against reality before quoting a number. Each row
  carries `input_tokens observed` — real token accounting from the live run,
  cached tokens included — next to `residency × turns`. `net_tokens` is a
  structural FLOOR, so observed input tokens *below* the modelled cost mean the
  residency figure does not describe that run at all; below
  `MIN_OBSERVED_MODEL_RATIO` (0.5) no verdict is printed for that row. In the
  other direction, above `MAX_OBSERVED_MODEL_RATIO` (5.0) the verdict is still
  printed but tagged, so a reader can see that the quoted number is a fraction
  of the real bill.

As of this writing **no live run has been performed**: `bench-results/`
contains only `e2e-residency-*.json` files. Layer 2 and Layer 3 are built and
tested against fixtures but have produced no real A/B data yet — do not read
anything in this document as a live-session finding until `make bench-e2e-live`
has actually been run and its output joined with `abreport.py`.

### 9.5 Two caveats in the residency numbers

**`residency_tokens` is an estimate, not a real tokenizer count.** It is
`ceil(payload_bytes / 4)` from `reporter.NewEstimator()` — the same `chars/4`
heuristic used everywhere else in this document (§2.1), not tokenizer output
from any specific model. Treat every `_tokens` figure in §9.3 as a byte count
in tokenizer's clothing.

**The synthetic ballast tools bias the measurement toward lazy mode's
weaker half.** Each ballast tool carries a realistic ~568-character
description (`tests/bench/e2e/fixtures/ballast.json`, embedded into Layer 1
as `BallastToolDescription` and read by Layer 2 from the same file; sized
against real tool caches — see below) but a deliberately trivial
one-property `inputSchema`, so `compactSchema` (LeanProxy's structural
schema compaction) never has anything to compact. Concretely: for one
ballast tool, the JSON-encoded description is 570 of the tool object's 675
bytes — about **84%** of every ballast tool is description text. Real tool
schemas measured from this repo's own tool caches
(`~/.config/leanproxy/toolcache/`) put description at a much smaller share
of total bytes: `codebase-memory.json` averages 1449 bytes/tool with a
610-character median description (~42%); `context7.json` averages 2240
bytes/tool with a 1218-character median description (~54%). So the harness
measures the description-truncation half of `lazy` mode's saving fairly, but
**understates** the schema-compaction half — real servers with richer
`inputSchema` objects would show `lazy` saving more than this sweep reports.
