# E2E A/B Benchmark Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a harness that measures real per-turn token cost across three proxy arms (`native` / `router` / `lazy`) at a swept MCP schema weight, replacing the accounting model in `token_economy_bench_test.go` for the question of whether the proxy pays for itself.

**Architecture:** Two independent layers. Layer 1 is a Go test that starts the real proxy over stdio against synthetic ballast servers and captures the exact `tools/list` bytes each arm delivers — deterministic, free, CI-safe. Layer 2 is a Python script that drives real Bob sessions through each arm and reads ground-truth token counts back out of Bob's SQLite store — coin-spending, env-gated. Layer 3 is a reporting step that combines them without silently interpolating.

**Tech Stack:** Go 1.25.5 (`github.com/mmornati/leanproxy-mcp`), Python 3.9+ stdlib only, SQLite, existing `tests/bench/mockmcp` and `pkg/reporter`.

**Spec:** `docs/superpowers/specs/2026-08-28-e2e-ab-harness-design.md`

## Global Constraints

- Go module is `github.com/mmornati/leanproxy-mcp`; Go 1.25.5.
- Python is stdlib-only, 3.9+ compatible — matches the `scripts/bobstat.py` convention. No pip dependencies.
- All tokenisation goes through `reporter.NewEstimator()` (`pkg/reporter/cost.go:82`). Never hand-roll a token count; the runtime cost tracker uses this primitive and the numbers must not diverge from `leanproxy-mcp savings`.
- `Estimator.EstimateTokens(content string) int` returns `ceil(len(content) / 4)`. `DefaultCharsPerToken = 4`.
- Layer 1 must not touch the network, must not read `~/.config/leanproxy_servers.yaml`, and must not spend coins. It runs in CI.
- Layer 2 runs only when `LEANPROXY_AB_LIVE=1` is set. Without it, exit 0 with a message.
- Layer 2 must restore `~/.bob/settings/mcp.json` unconditionally — on success, exception, and SIGINT/SIGTERM.
- Arm configs are written to temp dirs and passed via `--config`. Never mutate the user's real config.
- Report tokens and cost per task. Do not report ns/op — per project convention, speed is not the metric under study.
- The proxy binary under test is built from source into a temp dir, not taken from `~/.local/bin` or `dist/`.

## File Structure

**Layer 1 (Go, new package `tests/bench/e2e`):**

| File | Responsibility |
|---|---|
| `tests/bench/e2e/client.go` | Minimal stdio JSON-RPC MCP client: spawn a subprocess, `initialize`, `tools/list`, close. |
| `tests/bench/e2e/ballast.go` | Ballast server specs and leanproxy config generation. |
| `tests/bench/e2e/arms.go` | The three arm definitions; captures each arm's `tools/list` payload. |
| `tests/bench/e2e/report.go` | `Record` type and JSON serialisation, shared shape with Layer 2. |
| `tests/bench/e2e/residency_test.go` | The sweep test; writes `bench-results/e2e-<ts>.json`. |

**Layer 2 (Python):**

| File | Responsibility |
|---|---|
| `scripts/abbench.py` | Live A/B harness: config swap safety, Bob driver, SQLite readback, paired reporting. |
| `tests/bench/e2e/fixtures/tasks.json` | Frozen task set: prompts plus success assertions. |
| `tests/bench/e2e/test_abbench.py` | stdlib `unittest` tests for the pure functions in `abbench.py`. |

**Layer 3 (Python):**

| File | Responsibility |
|---|---|
| `scripts/abreport.py` | Join the two layers into a net-tokens curve; label measured vs derived; print breakeven. |
| `tests/bench/e2e/test_abreport.py` | stdlib `unittest` tests for `combine`. |

**Modified:**

| File | Change |
|---|---|
| `Makefile` | Add `bench-e2e` and `bench-e2e-live` targets. |

---

### Task 1: Stdio MCP client

The harness needs to speak MCP to a subprocess. `tests/e2e/helper_test.go` has process lifecycle helpers (`startServe`, `waitForServeReady`) but no JSON-RPC client, so this is new.

**Files:**
- Create: `tests/bench/e2e/client.go`
- Test: `tests/bench/e2e/client_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func Dial(bin string, args ...string) (*Client, error)`
  - `func (c *Client) Initialize() error`
  - `func (c *Client) ToolsListRaw() ([]byte, error)` — returns the raw `result` object bytes
  - `func (c *Client) Close() error`

- [ ] **Step 1: Write the failing test**

```go
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildMockMCP compiles the standalone mock MCP server and returns its path.
func buildMockMCP(t testing.TB) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockmcp")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/mmornati/leanproxy-mcp/tests/bench/mockmcp/cmd")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build mockmcp: %v", err)
	}
	return bin
}

func TestClientToolsList(t *testing.T) {
	bin := buildMockMCP(t)

	c, err := Dial(bin, "--tools=3")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	raw, err := c.ToolsListRaw()
	if err != nil {
		t.Fatalf("ToolsListRaw: %v", err)
	}

	got := strings.Count(string(raw), `"name"`)
	if got != 3 {
		t.Fatalf("expected 3 tools in payload, got %d: %s", got, raw)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tests/bench/e2e/ -run TestClientToolsList -v`
Expected: FAIL — `undefined: Dial`

- [ ] **Step 3: Write minimal implementation**

```go
// Package e2e contains the end-to-end A/B benchmark harness. Unlike
// tests/bench/token_economy_bench_test.go, which is a pure accounting model,
// this package starts the real proxy and measures the bytes a client actually
// receives.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
)

// Client is a minimal stdio JSON-RPC MCP client. It speaks just enough of the
// protocol to capture a tools/list payload: initialize, initialized, tools/list.
type Client struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	next int
}

// Dial starts bin with args and attaches to its stdin/stdout.
func Dial(bin string, args ...string) (*Client, error) {
	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	return &Client{cmd: cmd, in: stdin, out: bufio.NewReaderSize(stdout, 1<<20), next: 1}, nil
}

// call sends one JSON-RPC request and returns the raw `result` bytes.
func (c *Client) call(method string, params any) (json.RawMessage, error) {
	id := c.next
	c.next++

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", method, err)
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	line, err := c.out.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse %s response: %w (line: %s)", method, err, line)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s failed: %d %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// notify sends a JSON-RPC notification, which takes no response.
func (c *Client) notify(method string) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(body, '\n'))
	return err
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize() error {
	_, err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "leanproxy-ab-harness", "version": "1"},
	})
	if err != nil {
		return err
	}
	return c.notify("notifications/initialized")
}

// ToolsListRaw returns the raw result object from tools/list. This is the exact
// payload a client holds in context, which is the quantity under measurement.
func (c *Client) ToolsListRaw() ([]byte, error) {
	res, err := c.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Close shuts stdin and waits for the subprocess to exit.
func (c *Client) Close() error {
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tests/bench/e2e/ -run TestClientToolsList -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/bench/e2e/client.go tests/bench/e2e/client_test.go
git commit -m "test(bench): add stdio MCP client for the e2e A/B harness"
```

---

### Task 2: Ballast specs and config generation

Schema weight is the independent variable. `mockmcp.Config` already exposes `ToolCount`, so ballast is a precise dial. This task produces the server specs and the leanproxy YAML that points at them.

**Files:**
- Create: `tests/bench/e2e/ballast.go`
- Test: `tests/bench/e2e/ballast_test.go`

**Interfaces:**
- Consumes: `Dial`, `Initialize`, `ToolsListRaw` from Task 1.
- Produces:
  - `type Spec struct { Name string; Command string; Args []string }`
  - `func BallastSpecs(mockBin string, servers, toolsPerServer int) []Spec`
  - `func WriteConfig(dir string, specs []Spec) (string, error)`

- [ ] **Step 1: Write the failing test**

```go
package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestBallastSpecs(t *testing.T) {
	specs := BallastSpecs("/tmp/mockmcp", 3, 25)
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d", len(specs))
	}
	if specs[0].Name != "ballast0" {
		t.Errorf("expected ballast0, got %s", specs[0].Name)
	}
	if specs[2].Args[0] != "--tools=25" {
		t.Errorf("expected --tools=25, got %s", specs[2].Args[0])
	}
}

func TestBallastSpecsZero(t *testing.T) {
	if got := BallastSpecs("/tmp/mockmcp", 0, 25); len(got) != 0 {
		t.Fatalf("expected no specs at zero ballast, got %d", len(got))
	}
}

func TestWriteConfigIsLoadable(t *testing.T) {
	dir := t.TempDir()
	specs := BallastSpecs("/tmp/mockmcp", 2, 10)

	path, err := WriteConfig(dir, specs)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(raw)

	for _, want := range []string{`version: "1"`, "ballast0", "ballast1", "--tools=10", "transport: stdio", "enabled: true"} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tests/bench/e2e/ -run 'TestBallast|TestWriteConfig' -v`
Expected: FAIL — `undefined: BallastSpecs`

- [ ] **Step 3: Write minimal implementation**

```go
package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Spec describes one upstream MCP server participating in a sweep point.
// The same Spec is used two ways: the native arm dials it directly, and the
// proxy arms list it in a leanproxy config for the proxy to spawn.
type Spec struct {
	Name    string
	Command string
	Args    []string
}

// BallastSpecs returns `servers` synthetic MCP servers, each advertising
// `toolsPerServer` tools. Ballast exists to move total schema weight past the
// ~10 real tools in the production setup, so the sweep can reach the region
// where the proxy's floor and round trips are supposed to pay for themselves.
func BallastSpecs(mockBin string, servers, toolsPerServer int) []Spec {
	specs := make([]Spec, 0, servers)
	for i := 0; i < servers; i++ {
		specs = append(specs, Spec{
			Name:    fmt.Sprintf("ballast%d", i),
			Command: mockBin,
			Args:    []string{fmt.Sprintf("--tools=%d", toolsPerServer)},
		})
	}
	return specs
}

// WriteConfig writes a leanproxy_servers.yaml covering specs into dir and
// returns its path. The shape matches tests/e2e/helper_test.go:writeSimpleConfig.
func WriteConfig(dir string, specs []Spec) (string, error) {
	var b strings.Builder
	b.WriteString("version: \"1\"\nservers:\n")
	for _, s := range specs {
		b.WriteString(fmt.Sprintf("  - name: %s\n", s.Name))
		b.WriteString("    transport: stdio\n")
		b.WriteString("    enabled: true\n")
		b.WriteString("    stdio:\n")
		b.WriteString(fmt.Sprintf("      command: %s\n", s.Command))
		b.WriteString("      args: [")
		for i, a := range s.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("%q", a))
		}
		b.WriteString("]\n")
		b.WriteString("      env: []\n")
	}

	path := filepath.Join(dir, "leanproxy_servers.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return path, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tests/bench/e2e/ -run 'TestBallast|TestWriteConfig' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/bench/e2e/ballast.go tests/bench/e2e/ballast_test.go
git commit -m "test(bench): add ballast specs and config generation"
```

---

### Task 3: Arm capture

The three arms have different cost structures and must be measured separately — `router` is lazy loading (low residency, pays a discovery round trip), `lazy` is stub compression (moderate residency, no round trip). See spec Finding 1.

**Files:**
- Create: `tests/bench/e2e/arms.go`
- Test: `tests/bench/e2e/arms_test.go`

**Interfaces:**
- Consumes: `Dial`, `Initialize`, `ToolsListRaw` (Task 1); `Spec`, `WriteConfig` (Task 2).
- Produces:
  - `type Arm string`, constants `ArmNative`, `ArmRouter`, `ArmLazy`
  - `func AllArms() []Arm`
  - `func Capture(arm Arm, leanproxyBin string, specs []Spec, dir string) ([]byte, error)`

- [ ] **Step 1: Write the failing test**

```go
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildLeanproxy compiles the proxy under test. The binary is built from
// source rather than taken from ~/.local/bin so the measurement always matches
// the working tree.
func buildLeanproxy(t testing.TB) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "leanproxy-mcp")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/mmornati/leanproxy-mcp")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build leanproxy: %v", err)
	}
	return bin
}

func TestCaptureNativeConcatenatesServers(t *testing.T) {
	mock := buildMockMCP(t)
	specs := BallastSpecs(mock, 2, 5)

	payload, err := Capture(ArmNative, "", specs, t.TempDir())
	if err != nil {
		t.Fatalf("Capture native: %v", err)
	}
	// 2 servers x 5 tools = 10 tool entries held by the client.
	if got := strings.Count(string(payload), `"name"`); got != 10 {
		t.Fatalf("expected 10 tools in native payload, got %d", got)
	}
}

func TestCaptureRouterIsSmallerThanNative(t *testing.T) {
	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	specs := BallastSpecs(mock, 2, 25)

	native, err := Capture(ArmNative, "", specs, t.TempDir())
	if err != nil {
		t.Fatalf("Capture native: %v", err)
	}
	router, err := Capture(ArmRouter, lp, specs, t.TempDir())
	if err != nil {
		t.Fatalf("Capture router: %v", err)
	}

	if len(router) >= len(native) {
		t.Fatalf("router payload (%d B) should be smaller than native (%d B)", len(router), len(native))
	}
}

func TestCaptureLazySitsBetweenRouterAndNative(t *testing.T) {
	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	specs := BallastSpecs(mock, 2, 25)

	sizes := map[Arm]int{}
	for _, arm := range AllArms() {
		p, err := Capture(arm, lp, specs, t.TempDir())
		if err != nil {
			t.Fatalf("Capture %s: %v", arm, err)
		}
		sizes[arm] = len(p)
	}

	// router exposes 3 wrapper tools; lazy exposes one compact stub per tool;
	// native exposes every full schema.
	if !(sizes[ArmRouter] < sizes[ArmLazy] && sizes[ArmLazy] < sizes[ArmNative]) {
		t.Fatalf("expected router < lazy < native, got router=%d lazy=%d native=%d",
			sizes[ArmRouter], sizes[ArmLazy], sizes[ArmNative])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tests/bench/e2e/ -run TestCapture -v`
Expected: FAIL — `undefined: Capture`

- [ ] **Step 3: Write minimal implementation**

```go
package e2e

import (
	"bytes"
	"fmt"
)

// Arm is one of the three client-visible configurations under comparison.
type Arm string

const (
	// ArmNative is no proxy at all: every server's full schema reaches the client.
	ArmNative Arm = "native"
	// ArmRouter is `server run --stdio` without --lazy-tools: the client sees
	// only list_tools/invoke_tool/search_tools and must discover before it can
	// invoke. This is actual lazy loading.
	ArmRouter Arm = "router"
	// ArmLazy is `server run --stdio --lazy-tools`: every upstream tool appears
	// by prefixed name with a compact stub schema. No discovery round trip is
	// needed, so this is schema compression rather than lazy loading.
	ArmLazy Arm = "lazy"
)

// AllArms returns the arms in ascending order of expected residency cost.
func AllArms() []Arm { return []Arm{ArmRouter, ArmLazy, ArmNative} }

// Capture returns the exact tools/list payload a client holds under arm.
// For the proxy arms it starts the real leanproxy binary over stdio; for the
// native arm it dials each upstream directly and concatenates, which is what a
// client with those servers configured would carry.
func Capture(arm Arm, leanproxyBin string, specs []Spec, dir string) ([]byte, error) {
	if arm == ArmNative {
		return captureNative(specs)
	}

	cfg, err := WriteConfig(dir, specs)
	if err != nil {
		return nil, err
	}

	args := []string{"server", "run", "--stdio", "--config", cfg}
	if arm == ArmLazy {
		args = append(args, "--lazy-tools")
	}

	c, err := Dial(leanproxyBin, args...)
	if err != nil {
		return nil, fmt.Errorf("dial proxy for arm %s: %w", arm, err)
	}
	defer c.Close()

	if err := c.Initialize(); err != nil {
		return nil, fmt.Errorf("initialize arm %s: %w", arm, err)
	}
	return c.ToolsListRaw()
}

// captureNative dials every upstream directly and joins their tools/list
// results. The join is a concatenation of result objects rather than a merged
// array because we are measuring bytes held in context, not building a working
// tool list.
func captureNative(specs []Spec) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, s := range specs {
		c, err := Dial(s.Command, s.Args...)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", s.Name, err)
		}
		if err := c.Initialize(); err != nil {
			c.Close()
			return nil, fmt.Errorf("initialize %s: %w", s.Name, err)
		}
		raw, err := c.ToolsListRaw()
		c.Close()
		if err != nil {
			return nil, fmt.Errorf("tools/list %s: %w", s.Name, err)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tests/bench/e2e/ -run TestCapture -v`
Expected: PASS — all three ordering assertions hold.

If `TestCaptureLazySitsBetweenRouterAndNative` fails, do not adjust the assertion to match observed output. The ordering is a claim about how the proxy is supposed to behave; a violation is a finding about the proxy, and it should be reported rather than accommodated.

- [ ] **Step 5: Commit**

```bash
git add tests/bench/e2e/arms.go tests/bench/e2e/arms_test.go
git commit -m "test(bench): capture real tools/list payloads for all three arms"
```

---

### Task 4: Residency sweep, report, and Make target

Layer 1's deliverable. Produces the free half of the answer.

**Files:**
- Create: `tests/bench/e2e/report.go`
- Create: `tests/bench/e2e/residency_test.go`
- Modify: `Makefile` (append after the `bench-snapshot` target, which currently ends at line 127)

**Interfaces:**
- Consumes: `Capture`, `AllArms`, `BallastSpecs` (Tasks 2–3).
- Produces:
  - `type Record struct` with JSON tags — the shared record shape Layer 2 also emits
  - `func WriteReport(path string, recs []Record) error`

- [ ] **Step 1: Write the failing test**

```go
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReportRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	in := []Record{{
		Layer:           "residency",
		Arm:             "router",
		BallastServers:  2,
		BallastTools:    50,
		PayloadBytes:    1234,
		ResidencyTokens: 309,
	}}

	if err := WriteReport(path, in); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out []Record
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0].ResidencyTokens != 309 || out[0].Arm != "router" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tests/bench/e2e/ -run TestWriteReport -v`
Expected: FAIL — `undefined: Record`

- [ ] **Step 3: Write minimal implementation**

Create `tests/bench/e2e/report.go`:

```go
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
)

// Record is one measurement. Layer 1 (residency) and Layer 2 (live) both emit
// this shape so the two halves can be joined without a translation step.
// Fields not applicable to a layer stay zero.
type Record struct {
	// Layer is "residency" or "live".
	Layer string `json:"layer"`
	// Origin is "measured" or "derived". The harness never interpolates
	// silently; a derived point says so.
	Origin string `json:"origin"`
	Arm    string `json:"arm"`

	BallastServers int `json:"ballast_servers"`
	BallastTools   int `json:"ballast_tools"`

	// Residency fields (layer 1).
	PayloadBytes    int `json:"payload_bytes,omitempty"`
	ResidencyTokens int `json:"residency_tokens,omitempty"`

	// Live fields (layer 2).
	Task         string  `json:"task,omitempty"`
	Turns        int     `json:"turns,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CacheRead    int     `json:"cache_read,omitempty"`
	CacheWrite   int     `json:"cache_write,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	Succeeded    bool    `json:"succeeded,omitempty"`
}

// WriteReport serialises recs to path as indented JSON.
func WriteReport(path string, recs []Record) error {
	body, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tests/bench/e2e/ -run TestWriteReport -v`
Expected: PASS

- [ ] **Step 5: Write the sweep test**

Create `tests/bench/e2e/residency_test.go`:

```go
package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/reporter"
)

// ballastPoints is the sweep. The production setup contributes roughly 10 real
// tools (codebase-memory 8 after its include filter, context7 2), so the sweep
// reaches well past that to find where the proxy's floor plus round trips stop
// being worth paying.
//
// The small points are not padding. Task 3 measured the router arm's fixed
// wrapper floor at ~2174 B, which EXCEEDS a native payload below roughly 8
// tools — the router crossover sits between 4 and 8, and that crossover is the
// breakeven this harness exists to find. A sweep starting at 25 would step
// straight over it.
//
// Zero is deliberately absent: with no servers configured the proxy has nothing
// to proxy and `Capture(ArmRouter, ...)` fails with "read initialize: EOF".
var ballastPoints = []int{2, 4, 8, 25, 50, 100, 200}

func TestResidencySweep(t *testing.T) {
	if testing.Short() {
		t.Skip("residency sweep builds two binaries; skipped in -short")
	}

	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	est := reporter.NewEstimator()

	var recs []Record
	for _, tools := range ballastPoints {
		servers, perServer := 0, 0
		if tools > 0 {
			servers = 2 // split the load so the shape is not one giant server
			perServer = tools / servers
		}
		specs := BallastSpecs(mock, servers, perServer)
		// Record the tool count actually created, not the nominal sweep point:
		// integer division can drop a tool and the report must not claim
		// otherwise.
		actual := servers * perServer

		for _, arm := range AllArms() {
			payload, err := Capture(arm, lp, specs, t.TempDir())
			if err != nil {
				t.Fatalf("capture arm=%s tools=%d: %v", arm, tools, err)
			}
			recs = append(recs, Record{
				Layer:           "residency",
				Origin:          "measured",
				Arm:             string(arm),
				BallastServers:  servers,
				BallastTools:    actual,
				PayloadBytes:    len(payload),
				ResidencyTokens: est.EstimateTokens(string(payload)),
			})
			t.Logf("arm=%-7s ballast_tools=%3d bytes=%7d residency_tokens=%6d",
				arm, actual, len(payload), est.EstimateTokens(string(payload)))
		}
	}

	if err := os.MkdirAll("../../../bench-results", 0o755); err != nil {
		t.Fatalf("mkdir bench-results: %v", err)
	}
	out := filepath.Join("../../../bench-results",
		fmt.Sprintf("e2e-residency-%s.json", time.Now().Format("20060102-150405")))
	if err := WriteReport(out, recs); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	t.Logf("wrote %s", out)
}

// TestResidencyOrderingHoldsAcrossSweep asserts the cost ordering the design
// predicts, at every sweep point rather than at one.
func TestResidencyOrderingHoldsAcrossSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries; skipped in -short")
	}

	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	est := reporter.NewEstimator()

	for _, tools := range []int{50, 200} {
		specs := BallastSpecs(mock, 2, tools/2)
		got := map[Arm]int{}
		for _, arm := range AllArms() {
			payload, err := Capture(arm, lp, specs, t.TempDir())
			if err != nil {
				t.Fatalf("capture arm=%s: %v", arm, err)
			}
			got[arm] = est.EstimateTokens(string(payload))
		}
		if !(got[ArmRouter] < got[ArmLazy] && got[ArmLazy] < got[ArmNative]) {
			t.Errorf("ballast=%d: expected router < lazy < native, got router=%d lazy=%d native=%d",
				tools, got[ArmRouter], got[ArmLazy], got[ArmNative])
		}
	}
}
```

- [ ] **Step 6: Run the sweep**

Run: `go test ./tests/bench/e2e/ -run 'TestResidency' -v`
Expected: PASS, with a logged table of residency tokens per arm per sweep point, and a JSON file in `bench-results/`.

- [ ] **Step 7: Add the Make targets**

Append to `Makefile`:

```makefile
.PHONY: bench-e2e
bench-e2e: ## Run the free residency sweep (no LLM, no coins, CI-safe)
	@echo "Running e2e residency sweep across all three arms..."
	@mkdir -p bench-results
	$(GO) test ./tests/bench/e2e/ -run 'TestResidency' -v -timeout 10m

.PHONY: bench-e2e-live
bench-e2e-live: ## Run the live A/B sweep (SPENDS COINS; requires LEANPROXY_AB_LIVE=1)
	@echo "Running live A/B sweep — this spends coins."
	LEANPROXY_AB_LIVE=1 python3 scripts/abbench.py --out bench-results
```

- [ ] **Step 8: Verify the target works**

Run: `make bench-e2e`
Expected: sweep runs, `bench-results/e2e-residency-*.json` written.

- [ ] **Step 9: Commit**

```bash
git add tests/bench/e2e/report.go tests/bench/e2e/residency_test.go Makefile
git commit -m "feat(bench): add residency sweep and bench-e2e target"
```

---

### Task 5: Config swap safety and confound guard

Layer 2 begins here. This task builds only the safety layer, because a bug here breaks the user's Bob install. It is separately reviewable and separately testable.

Per spec Finding 2, `context7` is currently enabled in both `~/.bob/settings/mcp.json` and `~/.config/leanproxy_servers.yaml:31`, so Bob double-loads it. Measuring in that state is invalid.

**Files:**
- Create: `scripts/abbench.py`
- Test: `tests/bench/e2e/test_abbench.py`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `def detect_confound(bob_cfg: dict, lp_cfg_text: str) -> list[str]`
  - `def arm_config(arm: str, leanproxy_bin: str, base: dict) -> dict`
  - `class ConfigSwap` — context manager, restores on any exit path

- [ ] **Step 1: Write the failing test**

```python
"""Tests for the pure functions in scripts/abbench.py."""

import importlib.util
import json
import os
import pathlib
import tempfile
import unittest

_SPEC = importlib.util.spec_from_file_location(
    "abbench",
    pathlib.Path(__file__).resolve().parents[3] / "scripts" / "abbench.py",
)
abbench = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(abbench)


class TestDetectConfound(unittest.TestCase):
    def test_flags_server_enabled_in_both_places(self):
        bob = {"mcpServers": {"context7": {"disabled": False}, "leanproxy": {"disabled": False}}}
        lp = 'servers:\n  - name: context7\n    enabled: true\n'
        problems = abbench.detect_confound(bob, lp)
        self.assertTrue(any("context7" in p for p in problems), problems)

    def test_clean_config_has_no_problems(self):
        bob = {"mcpServers": {"leanproxy": {"disabled": False}}}
        lp = 'servers:\n  - name: context7\n    enabled: true\n'
        self.assertEqual(abbench.detect_confound(bob, lp), [])

    def test_disabled_in_bob_is_not_a_confound(self):
        bob = {"mcpServers": {"context7": {"disabled": True}, "leanproxy": {"disabled": False}}}
        lp = 'servers:\n  - name: context7\n    enabled: true\n'
        self.assertEqual(abbench.detect_confound(bob, lp), [])


class TestArmConfig(unittest.TestCase):
    BASE = {"mcpServers": {"leanproxy": {"command": "/old/bin", "args": ["server", "run", "--stdio"]}}}

    def test_router_arm_omits_lazy_tools(self):
        cfg = abbench.arm_config("router", "/new/bin", self.BASE)
        args = cfg["mcpServers"]["leanproxy"]["args"]
        self.assertIn("--stdio", args)
        self.assertNotIn("--lazy-tools", args)

    def test_lazy_arm_includes_lazy_tools(self):
        cfg = abbench.arm_config("lazy", "/new/bin", self.BASE)
        self.assertIn("--lazy-tools", cfg["mcpServers"]["leanproxy"]["args"])

    def test_native_arm_removes_the_proxy_entirely(self):
        cfg = abbench.arm_config("native", "/new/bin", self.BASE)
        self.assertNotIn("leanproxy", cfg["mcpServers"])

    def test_does_not_mutate_base(self):
        abbench.arm_config("lazy", "/new/bin", self.BASE)
        self.assertNotIn("--lazy-tools", self.BASE["mcpServers"]["leanproxy"]["args"])


class TestConfigSwap(unittest.TestCase):
    def _write(self, path, obj):
        with open(path, "w") as fh:
            json.dump(obj, fh)

    def test_restores_on_clean_exit(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})
            with abbench.ConfigSwap(p, {"swapped": True}):
                with open(p) as fh:
                    self.assertEqual(json.load(fh), {"swapped": True})
            with open(p) as fh:
                self.assertEqual(json.load(fh), {"original": True})

    def test_restores_on_exception(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})
            with self.assertRaises(RuntimeError):
                with abbench.ConfigSwap(p, {"swapped": True}):
                    raise RuntimeError("boom")
            with open(p) as fh:
                self.assertEqual(json.load(fh), {"original": True})


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: FAIL — `FileNotFoundError` for `scripts/abbench.py`

- [ ] **Step 3: Write minimal implementation**

Create `scripts/abbench.py`:

```python
#!/usr/bin/env python3
"""abbench — live A/B benchmark for LeanProxy across three arms.

Measures what the residency sweep cannot: how many turns a real model takes,
and whether it finds the tools it needs at all. Layer 1
(tests/bench/e2e/residency_test.go) covers residency for free; this script
covers behaviour, and it spends coins to do it.

    LEANPROXY_AB_LIVE=1 python3 scripts/abbench.py --out bench-results

Python 3.9+, standard library only.
"""

from __future__ import annotations

import argparse
import copy
import json
import os
import re
import shutil
import signal
import sys

HOME = os.path.expanduser("~")
DEFAULT_BOB_CFG = os.path.join(HOME, ".bob", "settings", "mcp.json")
DEFAULT_LP_CFG = os.path.join(HOME, ".config", "leanproxy_servers.yaml")
DEFAULT_DB = os.path.join(HOME, ".bob", "db", "bob.db")

ARMS = ("native", "router", "lazy")


# ---------------------------------------------------------------------------
# confound detection
# ---------------------------------------------------------------------------


def _lp_enabled_servers(lp_cfg_text: str) -> list:
    """Names of servers marked enabled in a leanproxy_servers.yaml.

    Deliberately a regex rather than a YAML parse: this module is stdlib-only
    and the shape being matched is a flat `- name:` / `enabled:` pair list.
    """
    names = []
    current = None
    for line in lp_cfg_text.splitlines():
        m = re.match(r"\s*-\s+name:\s*(\S+)", line)
        if m:
            current = m.group(1).strip("\"'")
            continue
        if current and re.match(r"\s*enabled:\s*true\b", line):
            names.append(current)
            current = None
    return names


def detect_confound(bob_cfg: dict, lp_cfg_text: str) -> list:
    """Servers reachable both directly from Bob and through the proxy.

    A server loaded twice inflates baseline schema weight and confounds every
    arm, so the harness refuses to run until this is resolved.
    """
    proxied = set(_lp_enabled_servers(lp_cfg_text))
    problems = []
    for name, entry in (bob_cfg.get("mcpServers") or {}).items():
        if name == "leanproxy":
            continue
        if entry.get("disabled") is True or entry.get("enabled") is False:
            continue
        if name in proxied:
            problems.append(
                f"{name!r} is enabled directly in Bob and also proxied by leanproxy "
                f"— it would be loaded twice"
            )
    return problems


# ---------------------------------------------------------------------------
# arm configuration
# ---------------------------------------------------------------------------


def arm_config(arm: str, leanproxy_bin: str, base: dict) -> dict:
    """Return a Bob mcp.json for the given arm, leaving `base` untouched."""
    if arm not in ARMS:
        raise ValueError(f"unknown arm: {arm}")

    cfg = copy.deepcopy(base)
    servers = cfg.setdefault("mcpServers", {})

    if arm == "native":
        servers.pop("leanproxy", None)
        return cfg

    args = ["server", "run", "--stdio", "--log-file", "/tmp/leanproxy-abbench.log"]
    if arm == "lazy":
        args.insert(3, "--lazy-tools")

    servers["leanproxy"] = {
        "command": leanproxy_bin,
        "args": args,
        "disabled": False,
        "enabled": True,
    }
    return cfg


# ---------------------------------------------------------------------------
# config swap
# ---------------------------------------------------------------------------


class ConfigSwap:
    """Swap a JSON config into place, restoring it on every exit path.

    Restores on normal exit, on exception, and on SIGINT/SIGTERM. A
    half-swapped config leaves Bob broken, so restoration is unconditional.
    """

    def __init__(self, path: str, new_cfg: dict):
        self.path = path
        self.new_cfg = new_cfg
        self.backup = path + ".abbench-backup"
        self._prev_handlers = {}

    def __enter__(self):
        shutil.copy2(self.path, self.backup)
        for sig in (signal.SIGINT, signal.SIGTERM):
            self._prev_handlers[sig] = signal.signal(sig, self._on_signal)
        with open(self.path, "w") as fh:
            json.dump(self.new_cfg, fh, indent=2)
        return self

    def _on_signal(self, signum, frame):
        self._restore()
        sys.exit(128 + signum)

    def _restore(self):
        if os.path.exists(self.backup):
            shutil.move(self.backup, self.path)
        for sig, handler in self._prev_handlers.items():
            if handler is not None:
                signal.signal(sig, handler)
        self._prev_handlers.clear()

    def __exit__(self, exc_type, exc, tb):
        self._restore()
        return False


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default="bench-results")
    ap.add_argument("--bob-config", default=DEFAULT_BOB_CFG)
    ap.add_argument("--lp-config", default=DEFAULT_LP_CFG)
    args = ap.parse_args(argv)

    if os.environ.get("LEANPROXY_AB_LIVE") != "1":
        print("abbench spends coins; set LEANPROXY_AB_LIVE=1 to run it.")
        return 0

    with open(args.bob_config) as fh:
        bob_cfg = json.load(fh)
    with open(args.lp_config) as fh:
        lp_text = fh.read()

    problems = detect_confound(bob_cfg, lp_text)
    if problems:
        print("Refusing to run — config confounds detected:", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        return 2

    print("config clean; runner lands in the next task")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: PASS — 9 tests.

- [ ] **Step 5: Verify the guard fires on the real config**

Run: `LEANPROXY_AB_LIVE=1 python3 scripts/abbench.py`
Expected: exit 2, reporting that `context7` is loaded both directly and through the proxy. This confirms spec Finding 2 against the live config. Report the output; do not fix the user's config as part of this task.

- [ ] **Step 6: Commit**

```bash
git add scripts/abbench.py tests/bench/e2e/test_abbench.py
git commit -m "feat(bench): add abbench config swap safety and confound guard"
```

---

### Task 6: Task fixture and Bob driver

**Files:**
- Create: `tests/bench/e2e/fixtures/tasks.json`
- Modify: `scripts/abbench.py` (add `run_task`, `read_task_result`; extend `main`)
- Modify: `tests/bench/e2e/test_abbench.py` (add fixture and readback tests)

**Interfaces:**
- Consumes: `ConfigSwap`, `arm_config` (Task 5).
- Produces:
  - `def load_tasks(path: str) -> list`
  - `def run_task(prompt: str, cwd: str, db_path: str, timeout: int = 900) -> str` — returns the new `task_id`
  - `def read_task_result(db_path: str, task_id: str, expect_tool: str) -> dict` — keys: `task_id`, `input_tokens`, `output_tokens`, `cache_read`, `cache_write`, `cost_usd`, `context_tokens`, `turns`, `succeeded`

- [ ] **Step 1: Select the five tasks**

Query real history for candidate prompts:

```bash
sqlite3 ~/.bob/db/bob.db \
  "select t.id, substr(t.first_message,1,90), json_extract(t.costs,'\$.cost')
   from tasks t
   where json_extract(t.costs,'\$.input') > 0
   order by json_extract(t.costs,'\$.cost') desc limit 25;"
```

Choose five against the spec's criteria: each must exercise at least one real server, each must have a deterministic success assertion (a named tool that must appear in the task's `role='tool'` rows), and the set must span the observed cost range rather than clustering at one difficulty.

Write `tests/bench/e2e/fixtures/tasks.json`:

```json
{
  "version": 1,
  "note": "Frozen task set for the live A/B sweep. Chosen from real recorded history so round trips are genuine. Do not edit without re-running every arm — runs are only comparable against the same fixture.",
  "tasks": [
    {
      "id": "graph-arch",
      "prompt": "Using the codebase graph, summarize the architecture of the pkg/mcp package.",
      "expect_tool": "codebase-memory_get_architecture"
    },
    {
      "id": "graph-callers",
      "prompt": "Who calls the truncation stats tracker in pkg/mcp? Trace the call path.",
      "expect_tool": "codebase-memory_trace_path"
    },
    {
      "id": "graph-search",
      "prompt": "Find every symbol in this repo whose name mentions estimator.",
      "expect_tool": "codebase-memory_search_graph"
    },
    {
      "id": "graph-snippet",
      "prompt": "Show me the source of the EstimateTokens function.",
      "expect_tool": "codebase-memory_get_code_snippet"
    },
    {
      "id": "graph-coverage",
      "prompt": "Check index coverage for pkg/reporter/cost.go and report any gaps.",
      "expect_tool": "codebase-memory_check_index_coverage"
    }
  ]
}
```

Note: `expect_tool` names are the proxied form. Under the `native` arm the prefix differs, so `read_task_result` matches on suffix rather than exact equality — see Step 3.

- [ ] **Step 2: Write the failing test**

Append to `tests/bench/e2e/test_abbench.py`:

```python
class TestLoadTasks(unittest.TestCase):
    def test_loads_frozen_fixture(self):
        path = pathlib.Path(__file__).resolve().parent / "fixtures" / "tasks.json"
        tasks = abbench.load_tasks(str(path))
        self.assertEqual(len(tasks), 5)
        for t in tasks:
            self.assertIn("id", t)
            self.assertIn("prompt", t)
            self.assertIn("expect_tool", t)

    def test_rejects_duplicate_ids(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "tasks.json")
            with open(p, "w") as fh:
                json.dump({"tasks": [
                    {"id": "a", "prompt": "x", "expect_tool": "t"},
                    {"id": "a", "prompt": "y", "expect_tool": "t"},
                ]}, fh)
            with self.assertRaises(ValueError):
                abbench.load_tasks(p)


class TestReadTaskResult(unittest.TestCase):
    def _db(self, d):
        import sqlite3
        p = os.path.join(d, "bob.db")
        conn = sqlite3.connect(p)
        conn.execute("create table tasks (id text, costs text)")
        conn.execute("create table messages (task_id text, role text, data text, created_at int)")
        conn.execute(
            "insert into tasks values (?,?)",
            ("t1", json.dumps({"input": 1000, "output": 50, "cacheRead": 800,
                               "cacheWrite": 200, "cost": 0.21, "contextTokens": 900})),
        )
        for i, role in enumerate(["user", "assistant", "tool", "assistant"]):
            data = json.dumps({"role": role, "name": "codebase-memory_get_architecture"}) \
                if role == "tool" else json.dumps({"role": role})
            conn.execute("insert into messages values (?,?,?,?)", ("t1", role, data, i))
        conn.commit()
        conn.close()
        return p

    def test_extracts_tokens_and_turns(self):
        with tempfile.TemporaryDirectory() as d:
            res = abbench.read_task_result(self._db(d), "t1", "codebase-memory_get_architecture")
            self.assertEqual(res["input_tokens"], 1000)
            self.assertEqual(res["output_tokens"], 50)
            self.assertEqual(res["cost_usd"], 0.21)
            self.assertEqual(res["turns"], 2)
            self.assertTrue(res["succeeded"])

    def test_marks_failure_when_expected_tool_absent(self):
        with tempfile.TemporaryDirectory() as d:
            res = abbench.read_task_result(self._db(d), "t1", "codebase-memory_trace_path")
            self.assertFalse(res["succeeded"])

    def test_matches_on_suffix_so_arms_with_different_prefixes_compare(self):
        with tempfile.TemporaryDirectory() as d:
            res = abbench.read_task_result(self._db(d), "t1", "get_architecture")
            self.assertTrue(res["succeeded"])
```

- [ ] **Step 3: Run test to verify it fails**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: FAIL — `AttributeError: module 'abbench' has no attribute 'load_tasks'`

- [ ] **Step 4: Write minimal implementation**

Add to `scripts/abbench.py`, above `main`:

```python
import sqlite3
import subprocess
import time


def load_tasks(path: str) -> list:
    """Load and validate the frozen task fixture."""
    with open(path) as fh:
        doc = json.load(fh)
    tasks = doc.get("tasks") or []
    seen = set()
    for t in tasks:
        for key in ("id", "prompt", "expect_tool"):
            if key not in t:
                raise ValueError(f"task missing {key!r}: {t}")
        if t["id"] in seen:
            raise ValueError(f"duplicate task id: {t['id']}")
        seen.add(t["id"])
    return tasks


def _latest_task_id(db_path: str) -> str:
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        row = conn.execute("select id from tasks order by created_at desc limit 1").fetchone()
    finally:
        conn.close()
    return row[0] if row else None


def run_task(prompt: str, cwd: str, db_path: str, timeout: int = 900) -> str:
    """Run one prompt through Bob headlessly and return the new task id.

    The id is recovered by diffing the newest task row before and after, since
    `bob run` does not print it in a machine-readable form.
    """
    before = _latest_task_id(db_path)

    proc = subprocess.run(
        ["bob", "run", prompt],
        cwd=cwd,
        capture_output=True,
        text=True,
        timeout=timeout,
    )

    # Poll briefly: Bob writes the task row asynchronously on completion.
    deadline = time.time() + 30
    while time.time() < deadline:
        after = _latest_task_id(db_path)
        if after and after != before:
            return after
        time.sleep(1)

    raise RuntimeError(
        f"no new task row appeared after `bob run` (exit {proc.returncode})\n"
        f"stdout: {proc.stdout[-500:]}\nstderr: {proc.stderr[-500:]}"
    )


def read_task_result(db_path: str, task_id: str, expect_tool: str) -> dict:
    """Read ground-truth token counts, turn count, and success for one task.

    Turn count is the number of assistant messages: each one is a model call
    that re-sends the whole conversation, which is exactly the cost lazy
    loading trades residency for.

    Success matches `expect_tool` as a substring of the recorded tool message,
    because the same tool is named `codebase-memory_get_architecture` behind the
    proxy and `get_architecture` natively. Substring rather than equality keeps
    the arms comparable; fixtures should therefore name the unprefixed tool.
    """
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        row = conn.execute("select costs from tasks where id = ?", (task_id,)).fetchone()
        costs = json.loads(row[0]) if row and row[0] else {}

        turns = conn.execute(
            "select count(*) from messages where task_id = ? and role = 'assistant'",
            (task_id,),
        ).fetchone()[0]

        tool_rows = conn.execute(
            "select data from messages where task_id = ? and role = 'tool'",
            (task_id,),
        ).fetchall()
    finally:
        conn.close()

    succeeded = any(expect_tool in (r[0] or "") for r in tool_rows)

    return {
        "task_id": task_id,
        "input_tokens": costs.get("input", 0),
        "output_tokens": costs.get("output", 0),
        "cache_read": costs.get("cacheRead", 0),
        "cache_write": costs.get("cacheWrite", 0),
        "cost_usd": costs.get("cost", 0.0),
        "context_tokens": costs.get("contextTokens", 0),
        "turns": turns,
        "succeeded": succeeded,
    }
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: PASS — 15 tests.

- [ ] **Step 6: Commit**

```bash
git add scripts/abbench.py tests/bench/e2e/fixtures/tasks.json tests/bench/e2e/test_abbench.py
git commit -m "feat(bench): add frozen task fixture and Bob driver with SQLite readback"
```

---

### Task 7: Paired analysis and live sweep wiring

The final Layer 2 deliverable. Per the spec, paired per-task deltas are the primary statistic — aggregate means are reported but flagged, because observed per-task cost spans $0.10–$1.30 and would swamp the effect.

**Files:**
- Modify: `scripts/abbench.py` (add `paired_deltas`, `summarize`; complete `main`)
- Modify: `tests/bench/e2e/test_abbench.py` (add analysis tests)

**Interfaces:**
- Consumes: `load_tasks`, `run_task`, `read_task_result`, `ConfigSwap`, `arm_config`.
- Produces:
  - `def paired_deltas(records: list, baseline: str, arm: str, field: str) -> dict`

- [ ] **Step 1: Write the failing test**

Append to `tests/bench/e2e/test_abbench.py`:

```python
class TestPairedDeltas(unittest.TestCase):
    RECORDS = [
        {"arm": "native", "task": "a", "cost_usd": 1.00, "turns": 3},
        {"arm": "native", "task": "b", "cost_usd": 0.10, "turns": 2},
        {"arm": "lazy",   "task": "a", "cost_usd": 0.80, "turns": 3},
        {"arm": "lazy",   "task": "b", "cost_usd": 0.08, "turns": 2},
    ]

    def test_pairs_by_task_and_reports_consistent_sign(self):
        out = abbench.paired_deltas(self.RECORDS, "native", "lazy", "cost_usd")
        self.assertAlmostEqual(out["deltas"]["a"], -0.20)
        self.assertAlmostEqual(out["deltas"]["b"], -0.02)
        self.assertTrue(out["consistent"])
        self.assertAlmostEqual(out["total_delta"], -0.22)

    def test_reports_inconsistent_when_signs_disagree(self):
        recs = self.RECORDS + [
            {"arm": "native", "task": "c", "cost_usd": 0.10, "turns": 1},
            {"arm": "lazy",   "task": "c", "cost_usd": 0.30, "turns": 3},
        ]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertFalse(out["consistent"])

    def test_ignores_tasks_missing_from_one_arm(self):
        recs = self.RECORDS + [{"arm": "native", "task": "z", "cost_usd": 5.0, "turns": 9}]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertNotIn("z", out["deltas"])
        self.assertEqual(out["pairs"], 2)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: FAIL — `module 'abbench' has no attribute 'paired_deltas'`

- [ ] **Step 3: Write minimal implementation**

Add to `scripts/abbench.py`:

```python
def paired_deltas(records: list, baseline: str, arm: str, field: str) -> dict:
    """Per-task deltas between two arms, paired on task id.

    Pairing is the primary statistic. Observed per-task cost spans an order of
    magnitude, so an unpaired mean over five tasks would report task difficulty
    rather than arm effect. Tasks present in only one arm are dropped: an
    unpaired point cannot contribute a delta.

    `consistent` reports whether every paired delta shares a sign. When it is
    False the caller should report "no detectable effect" rather than a point
    estimate.
    """
    by_arm = {}
    for r in records:
        by_arm.setdefault(r["arm"], {})[r["task"]] = r

    base = by_arm.get(baseline, {})
    other = by_arm.get(arm, {})
    shared = sorted(set(base) & set(other))

    deltas = {t: other[t][field] - base[t][field] for t in shared}
    signs = {d > 0 for d in deltas.values() if d != 0}

    return {
        "baseline": baseline,
        "arm": arm,
        "field": field,
        "pairs": len(shared),
        "deltas": deltas,
        "total_delta": sum(deltas.values()),
        "consistent": len(signs) <= 1 and len(deltas) > 0,
    }
```

Then replace the placeholder tail of `main` (the `print("config clean; ...")` line) with:

```python
    tasks = load_tasks(args.tasks)
    leanproxy_bin = args.leanproxy_bin
    records = []

    for arm in ARMS:
        cfg = arm_config(arm, leanproxy_bin, bob_cfg)
        with ConfigSwap(args.bob_config, cfg):
            for t in tasks:
                print(f"[{arm}] {t['id']} ...", flush=True)
                try:
                    task_id = run_task(t["prompt"], args.cwd, args.db)
                except Exception as exc:  # noqa: BLE001 - a failed run is data
                    print(f"  run failed: {exc}", file=sys.stderr)
                    records.append({"layer": "live", "origin": "measured", "arm": arm,
                                    "task": t["id"], "succeeded": False, "error": str(exc)})
                    continue
                res = read_task_result(args.db, task_id, t["expect_tool"])
                res.update({"layer": "live", "origin": "measured", "arm": arm, "task": t["id"]})
                records.append(res)
                print(f"  cost={res['cost_usd']:.4f} turns={res['turns']} ok={res['succeeded']}")

    os.makedirs(args.out, exist_ok=True)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    out_path = os.path.join(args.out, f"e2e-live-{stamp}.json")
    with open(out_path, "w") as fh:
        json.dump(records, fh, indent=2)

    print(f"\nwrote {out_path}\n")
    print("Paired per-task deltas vs native (primary statistic):")
    for arm in ("router", "lazy"):
        for field in ("cost_usd", "turns"):
            d = paired_deltas(records, "native", arm, field)
            if d["pairs"] == 0:
                continue
            verdict = f"{d['total_delta']:+.4f}" if d["consistent"] else "no detectable effect (signs disagree)"
            print(f"  {arm:<7} {field:<9} pairs={d['pairs']} {verdict}")

    failures = [r for r in records if not r.get("succeeded")]
    if failures:
        print(f"\n{len(failures)} task run(s) failed — discovery failures are a real "
              f"cost of lazy loading and are reported, not discarded:")
        for f in failures:
            print(f"  {f['arm']}/{f['task']}")

    return 0
```

Add the new arguments to the parser in `main`:

```python
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--cwd", default=os.getcwd())
    ap.add_argument("--leanproxy-bin", default=os.path.join(HOME, ".local", "bin", "leanproxy-mcp"))
    ap.add_argument("--tasks", default=os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..",
        "tests", "bench", "e2e", "fixtures", "tasks.json"))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: PASS — 18 tests.

- [ ] **Step 5: Verify the dry path still guards**

Run: `python3 scripts/abbench.py`
Expected: prints the coin warning, exits 0, spends nothing.

- [ ] **Step 6: Commit**

```bash
git add scripts/abbench.py tests/bench/e2e/test_abbench.py
git commit -m "feat(bench): add paired-delta analysis and live sweep wiring"
```

---

### Task 8: Live ballast sweep

The spec calls for live points at k=0 **and** k=100, not just the current setup. Ballast changes behaviour, not only residency: 100 extra irrelevant tools may change how many turns the model takes and whether it picks the right tool at all. Layer 1 cannot see either effect.

**Files:**
- Modify: `scripts/abbench.py` (add `ballast_servers`, extend `arm_config`, sweep in `main`)
- Modify: `tests/bench/e2e/test_abbench.py`

**Interfaces:**
- Consumes: `arm_config` (Task 5).
- Produces:
  - `def ballast_servers(mock_bin: str, servers: int, tools_per: int) -> dict`
  - `arm_config` gains a fourth parameter: `arm_config(arm, leanproxy_bin, base, ballast=None)`

- [ ] **Step 1: Write the failing test**

Append to `tests/bench/e2e/test_abbench.py`:

```python
class TestBallast(unittest.TestCase):
    def test_generates_named_ballast_entries(self):
        b = abbench.ballast_servers("/tmp/mockmcp", 2, 50)
        self.assertEqual(len(b), 2)
        self.assertIn("ballast0", b)
        self.assertIn("--tools=50", b["ballast0"]["args"])

    def test_zero_servers_yields_nothing(self):
        self.assertEqual(abbench.ballast_servers("/tmp/mockmcp", 0, 50), {})

    def test_arm_config_merges_ballast_into_native(self):
        base = {"mcpServers": {"leanproxy": {"command": "/old", "args": []}}}
        ballast = abbench.ballast_servers("/tmp/mockmcp", 2, 50)
        cfg = abbench.arm_config("native", "/new/bin", base, ballast)
        self.assertIn("ballast0", cfg["mcpServers"])
        self.assertNotIn("leanproxy", cfg["mcpServers"])

    def test_arm_config_without_ballast_is_unchanged(self):
        base = {"mcpServers": {"leanproxy": {"command": "/old", "args": []}}}
        cfg = abbench.arm_config("lazy", "/new/bin", base)
        self.assertEqual([k for k in cfg["mcpServers"] if k.startswith("ballast")], [])
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: FAIL — `module 'abbench' has no attribute 'ballast_servers'`

- [ ] **Step 3: Write minimal implementation**

Add to `scripts/abbench.py`:

```python
def ballast_servers(mock_bin: str, servers: int, tools_per: int) -> dict:
    """Bob mcpServers entries for synthetic ballast.

    Ballast exists to push total schema weight past the ~10 real tools so the
    live layer can be measured in the same region the residency sweep covers.
    """
    return {
        f"ballast{i}": {
            "command": mock_bin,
            "args": [f"--tools={tools_per}"],
            "disabled": False,
            "enabled": True,
        }
        for i in range(servers)
    }
```

Change the `arm_config` signature and add the merge. Replace the existing `def arm_config(arm: str, leanproxy_bin: str, base: dict) -> dict:` line and its `cfg`/`servers` setup with:

```python
def arm_config(arm: str, leanproxy_bin: str, base: dict, ballast: dict = None) -> dict:
    """Return a Bob mcp.json for the given arm, leaving `base` untouched.

    Ballast is attached directly to Bob in every arm. It is deliberately not
    routed through the proxy: its job is to add schema weight the model must
    carry, which is the same job in all three arms.
    """
    if arm not in ARMS:
        raise ValueError(f"unknown arm: {arm}")

    cfg = copy.deepcopy(base)
    servers = cfg.setdefault("mcpServers", {})
    if ballast:
        servers.update(copy.deepcopy(ballast))
```

The remainder of the function (the `native` early return and the proxy entry) is unchanged.

- [ ] **Step 4: Sweep ballast in `main`**

Add the arguments:

```python
    ap.add_argument("--ballast-points", default="0,100",
                    help="comma-separated total ballast tool counts to run live")
    ap.add_argument("--mock-bin", default="", help="path to the built mockmcp binary")
```

Wrap the existing arm loop in a ballast loop, replacing `for arm in ARMS:` with:

```python
    points = [int(x) for x in args.ballast_points.split(",") if x.strip()]
    for tools in points:
        servers = 2 if tools > 0 else 0
        per = tools // servers if servers else 0
        actual = servers * per
        if servers and not args.mock_bin:
            print("--mock-bin is required for non-zero ballast points", file=sys.stderr)
            return 2
        ballast = ballast_servers(args.mock_bin, servers, per)

        for arm in ARMS:
            cfg = arm_config(arm, leanproxy_bin, bob_cfg, ballast)
```

Inside, tag each record with the ballast level so points stay distinguishable, and make the pairing key include it:

```python
                res.update({"layer": "live", "origin": "measured", "arm": arm,
                            "task": f"{t['id']}@{actual}", "ballast_tools": actual})
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: PASS — 22 tests.

- [ ] **Step 6: Commit**

```bash
git add scripts/abbench.py tests/bench/e2e/test_abbench.py
git commit -m "feat(bench): sweep ballast weight in the live A/B layer"
```

---

### Task 9: Layer 3 combination report

Joins the two layers into the curve that answers the original question. Per the spec, points are labelled `measured` or `derived`; the harness never interpolates silently.

**Files:**
- Create: `scripts/abreport.py`
- Test: `tests/bench/e2e/test_abreport.py`

**Interfaces:**
- Consumes: the JSON emitted by Task 4 (`e2e-residency-*.json`) and Task 7 (`e2e-live-*.json`).
- Produces:
  - `def combine(residency: list, live: list) -> list`

- [ ] **Step 1: Write the failing test**

```python
"""Tests for scripts/abreport.py."""

import importlib.util
import pathlib
import unittest

_SPEC = importlib.util.spec_from_file_location(
    "abreport",
    pathlib.Path(__file__).resolve().parents[3] / "scripts" / "abreport.py",
)
abreport = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(abreport)


class TestCombine(unittest.TestCase):
    RESIDENCY = [
        {"layer": "residency", "arm": "lazy", "ballast_tools": 0, "residency_tokens": 500},
        {"layer": "residency", "arm": "lazy", "ballast_tools": 100, "residency_tokens": 3000},
        {"layer": "residency", "arm": "native", "ballast_tools": 0, "residency_tokens": 900},
        {"layer": "residency", "arm": "native", "ballast_tools": 100, "residency_tokens": 12000},
    ]
    LIVE = [
        {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 4, "output_tokens": 200},
        {"layer": "live", "arm": "native", "ballast_tools": 0, "turns": 3, "output_tokens": 150},
    ]

    def test_measured_points_use_real_turn_counts(self):
        rows = abreport.combine(self.RESIDENCY, self.LIVE)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["origin"], "measured")
        self.assertEqual(lazy0["net_tokens"], 500 * 4 + 200)

    def test_points_without_live_data_are_marked_derived(self):
        rows = abreport.combine(self.RESIDENCY, self.LIVE)
        lazy100 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 100][0]
        self.assertEqual(lazy100["origin"], "derived")

    def test_derived_points_reuse_the_arms_measured_turn_count(self):
        rows = abreport.combine(self.RESIDENCY, self.LIVE)
        lazy100 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 100][0]
        self.assertEqual(lazy100["net_tokens"], 3000 * 4 + 200)

    def test_arm_with_no_live_data_at_all_is_skipped(self):
        rows = abreport.combine(self.RESIDENCY, [])
        self.assertEqual(rows, [])


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: FAIL — `FileNotFoundError` for `scripts/abreport.py`

- [ ] **Step 3: Write minimal implementation**

Create `scripts/abreport.py`:

```python
#!/usr/bin/env python3
"""abreport — combine the residency sweep and the live A/B run into one curve.

    python3 scripts/abreport.py bench-results/e2e-residency-*.json \
                                bench-results/e2e-live-*.json

Python 3.9+, standard library only.
"""

from __future__ import annotations

import argparse
import json
import statistics
import sys


def combine(residency: list, live: list) -> list:
    """Join residency and live records into net tokens per task.

        net_tokens = residency_tokens x turns + output_tokens

    Residency is measured exactly at every sweep point. Turns and output come
    from the live layer, which runs at only some points. A point with live data
    at its own ballast level is `measured`; a point that borrows another
    level's turn count is `derived`. Arms with no live data anywhere are
    omitted rather than guessed at.
    """
    by_arm = {}
    for r in live:
        by_arm.setdefault(r["arm"], {}).setdefault(r.get("ballast_tools", 0), []).append(r)

    rows = []
    for res in residency:
        arm = res["arm"]
        if arm not in by_arm:
            continue

        level = res.get("ballast_tools", 0)
        levels = by_arm[arm]
        if level in levels:
            samples, origin = levels[level], "measured"
        else:
            # Borrow the arm's nearest measured level. Turn count is a property
            # of how the model works, not of ballast size, so this is a
            # defensible carry-over — but it is labelled, never silent.
            nearest = min(levels, key=lambda k: abs(k - level))
            samples, origin = levels[nearest], "derived"

        turns = statistics.mean(s["turns"] for s in samples)
        output = statistics.mean(s["output_tokens"] for s in samples)

        rows.append({
            "arm": arm,
            "ballast_tools": level,
            "origin": origin,
            "residency_tokens": res["residency_tokens"],
            "turns": turns,
            "output_tokens": output,
            "net_tokens": res["residency_tokens"] * turns + output,
        })
    return rows


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("residency")
    ap.add_argument("live")
    args = ap.parse_args(argv)

    with open(args.residency) as fh:
        residency = json.load(fh)
    with open(args.live) as fh:
        live = json.load(fh)

    rows = combine(residency, live)
    if not rows:
        print("no overlapping arms between the two files", file=sys.stderr)
        return 1

    rows.sort(key=lambda r: (r["ballast_tools"], r["arm"]))
    print(f"{'ballast':>8} {'arm':<8} {'origin':<9} {'residency':>10} {'turns':>6} {'net_tokens':>11}")
    for r in rows:
        print(f"{r['ballast_tools']:>8} {r['arm']:<8} {r['origin']:<9} "
              f"{r['residency_tokens']:>10} {r['turns']:>6.1f} {r['net_tokens']:>11.0f}")

    # Breakeven: the lowest ballast level at which a proxy arm beats native.
    by_level = {}
    for r in rows:
        by_level.setdefault(r["ballast_tools"], {})[r["arm"]] = r["net_tokens"]
    print()
    for level in sorted(by_level):
        native = by_level[level].get("native")
        if native is None:
            continue
        for arm in ("router", "lazy"):
            got = by_level[level].get(arm)
            if got is None:
                continue
            delta = got - native
            verdict = "saves" if delta < 0 else "COSTS MORE"
            print(f"  ballast={level:<4} {arm:<7} {verdict} {abs(delta):>9.0f} tok/task vs native")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v`
Expected: PASS — 26 tests.

- [ ] **Step 5: Commit**

```bash
git add scripts/abreport.py tests/bench/e2e/test_abreport.py
git commit -m "feat(bench): combine residency and live layers into a breakeven curve"
```

---

### Task 10: Documentation

**Files:**
- Modify: `docs/benchmark-results.md`
- Modify: `README.md:29-31` (the note under the benchmark table)

**Interfaces:**
- Consumes: everything.
- Produces: no code.

- [ ] **Step 1: Add a methodology section to `docs/benchmark-results.md`**

Append verbatim:

````markdown
## Modelled numbers vs. measured numbers

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

## The e2e harness

`tests/bench/e2e` measures instead of modelling. It starts the real proxy over
stdio and captures the exact `tools/list` bytes a client receives.

### Three arms, not two

| Arm | Config | tools/list contents | Discovery round trip |
|---|---|---|---|
| `native` | servers configured directly in the client | every full schema | none |
| `router` | `server run --stdio` | 3 wrapper tools | **required** |
| `lazy` | `server run --stdio --lazy-tools` | one compact stub per tool | none |

`router` is lazy loading: minimal residency, paid for with an extra turn.
`lazy` is schema compression: moderate residency, no extra turn. They have
opposite cost structures and must not be conflated.

### Running it

```bash
make bench-e2e        # residency sweep across all arms. No LLM, no coins.
make bench-e2e-live   # live A/B through real sessions. SPENDS COINS.
python3 scripts/abreport.py bench-results/e2e-residency-*.json \
                            bench-results/e2e-live-*.json
```

### Reading the live results

The primary statistic is the **paired per-task delta**: the same prompt run
under two arms, subtracted. Observed per-task cost spans an order of magnitude,
so an unpaired mean over a handful of tasks reports task difficulty rather than
arm effect. Aggregate means are printed too, and flagged as the weaker figure.
Where paired deltas disagree in sign, the harness reports "no detectable
effect" instead of a point estimate.

Failed runs are reported, not discarded. A model that never finds the tool it
needed is a real cost of lazy loading, and token accounting alone cannot see it.
````

- [ ] **Step 2: Qualify the README claim**

`README.md:29-31` currently reads:

> All numbers above come from `tests/bench/token_economy_bench_test.go` and `pkg/reporter/cost.go`. **Full results and methodology: [docs/benchmark-results.md](docs/benchmark-results.md).**

Replace with:

> All numbers above come from `tests/bench/token_economy_bench_test.go` and `pkg/reporter/cost.go`. They measure the **tool-schema slice** via an accounting model, not end-to-end session cost — the model omits discovery round trips and charges a single stub rather than the full manifest. For measured end-to-end numbers run `make bench-e2e`. **Full results and methodology, including what the modelled figures do and do not claim: [docs/benchmark-results.md](docs/benchmark-results.md).**

Do not change the headline table's numbers. They are correct for what they measure; the fix is stating what that is.

- [ ] **Step 3: Verify docs build**

Run: `mkdocs build --strict` (config at `mkdocs.yml`)
Expected: builds clean. If mkdocs is not installed, skip and note it.

- [ ] **Step 4: Commit**

```bash
git add docs/benchmark-results.md README.md
git commit -m "docs: distinguish modelled schema-tax numbers from measured e2e results"
```

---

## Verification

After all tasks:

```bash
make lint
make test
make bench-e2e
python3 -m unittest discover -s tests/bench/e2e -p 'test_*.py' -v
```

The live layer is run separately and deliberately, since it spends coins:

```bash
make bench-e2e-live
python3 scripts/abreport.py bench-results/e2e-residency-*.json bench-results/e2e-live-*.json
```

Expect `bench-e2e-live` to exit 2 until the context7 double-load from spec Finding 2 is resolved.

**Definition of done:** `make bench-e2e` produces a residency table showing `router < lazy < native` at every sweep point, and `abreport.py` prints a breakeven line stating, for each proxy arm, whether it saves or costs more tokens per task than native at each ballast level. That line is the answer to the question this harness was built for.
