package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mmornati/leanproxy-mcp/pkg/migrate"
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

	// The substring checks above only prove the text is present, not that
	// it parses. Load the file through the same production loader the
	// proxy uses (cmd/serve.go) to actually exercise loadability.
	cfg, err := migrate.LoadConfig(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig returned nil config")
	}
	if len(cfg.Servers) != len(specs) {
		t.Fatalf("expected %d servers, got %d", len(specs), len(cfg.Servers))
	}

	first := cfg.Servers[0]
	if first.Name != specs[0].Name {
		t.Errorf("expected server name %s, got %s", specs[0].Name, first.Name)
	}
	if first.Stdio == nil {
		t.Fatal("expected stdio config, got nil")
	}
	if first.Stdio.Command != specs[0].Command {
		t.Errorf("expected command %q, got %q", specs[0].Command, first.Stdio.Command)
	}
	if len(first.Stdio.Args) != len(specs[0].Args) || first.Stdio.Args[0] != specs[0].Args[0] {
		t.Errorf("expected args %v, got %v", specs[0].Args, first.Stdio.Args)
	}
}

// TestBallastToolIsRealisticWeight guards against the ballast schema silently
// degenerating back into the tiny stub that made TestCaptureLazySitsBetween-
// RouterAndNative fail: with a one-property inputSchema and a description
// under stubDescChars (pkg/mcp/discovery.go), lazy-mode compaction has
// nothing to strip and the "servername_" prefix alone makes lazy bigger than
// native. The realistic target comes from leanproxy's own persisted tool
// caches (~/.config/leanproxy/toolcache/codebase-memory.json: 610-char
// median description, 1449 bytes/tool average), NOT from
// tests/bench/fixtures/live-snapshot.json — that file is a seeded placeholder
// (see its own "source" field), not a measurement, and must never be cited as
// one. mockmcp's inputSchema stays a single string-typed property
// (deliberately, so compactSchema has nothing to compact — see task-3
// report), so a ballast tool's total bytes can't reach 1449; this test guards
// the one lever that matters here, description length, directly.
func TestBallastToolIsRealisticWeight(t *testing.T) {
	const wantDescMin, wantDescMax = 550, 700 // near codebase-memory's 610-char median
	if got := len(BallastToolDescription); got < wantDescMin || got > wantDescMax {
		t.Fatalf("BallastToolDescription is %d chars, want %d-%d (near codebase-memory's measured 610-char median)", got, wantDescMin, wantDescMax)
	}
	// The fixture's own recorded description_chars must describe the prose it
	// ships. mustLoadBallastFixture already panics on a mismatch at package
	// init; this asserts it as a test failure with the numbers in it, and
	// pins the shipped length so a silent re-write of the prose to a
	// different size shows up as a diff on a measured number rather than
	// only inside a comment (review M-1).
	if Ballast.DescriptionChars != 568 {
		t.Errorf("fixture description_chars = %d, want the measured 568 "+
			"(if the prose really changed, re-measure and update ballast.go's comment too)",
			Ballast.DescriptionChars)
	}

	mock := buildMockMCP(t)
	specs := BallastSpecs(mock, 1, 1)

	c, err := Dial(specs[0].Command, specs[0].Args...)
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

	// Well past the ~172-char arithmetic estimate (stubDescChars=160 +
	// ellipsis 3 + prefix 9) and the empirically measured 168-character flip
	// point (fixtures/ballast.json measured.lazy_native_flip_chars), so
	// truncation always has real prose to cut.
	const wantMin = 650
	if got := len(raw); got < wantMin {
		t.Fatalf("single-tool tools/list payload is %d bytes, want >= %d", got, wantMin)
	}
}

// TestWriteConfigCommandWithColonSpace is a regression test: a command path
// containing a colon-space sequence (e.g. "C:\ tools\mockmcp") used to be
// interpolated into the YAML as an unquoted plain scalar, which the real
// loader rejects with "mapping values are not allowed in this context".
// WriteConfig must quote Command the same way it already quotes Args.
func TestWriteConfigCommandWithColonSpace(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{
		Name:    "ballast0",
		Command: "/opt/weird: path/mockmcp",
		Args:    []string{"--tools=5"},
	}}

	path, err := WriteConfig(dir, specs)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	cfg, err := migrate.LoadConfig(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil || len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got config %+v", cfg)
	}
	if cfg.Servers[0].Stdio == nil {
		t.Fatal("expected stdio config, got nil")
	}
	if got := cfg.Servers[0].Stdio.Command; got != specs[0].Command {
		t.Errorf("expected command %q to round-trip, got %q", specs[0].Command, got)
	}
}

// layer2BallastArgs asks scripts/abbench.py what command line IT gives a
// ballast server, for both the agent-attached form (native arm) and the
// behind-the-proxy form (router/lazy arms). Layer 2 is Python and Layer 1 is
// Go, so the only way to compare them is to run the other layer.
func layer2BallastArgs(t *testing.T, mockBin string, tools int) (bob, lp []string) {
	t.Helper()

	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not on PATH, so the cross-layer ballast weight guard did not run: %v", err)
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	script := filepath.Join(root, "scripts", "abbench.py")
	fixture := filepath.Join(root, "tests", "bench", "e2e", "fixtures", "ballast.json")

	// script and fixture are read by the python3 subprocess below, not by
	// this test binary, so `go test`'s result cache — which keys on the
	// files THIS process reads — has no way to see that either changed. An
	// edit to scripts/abbench.py alone would otherwise return a stale PASS
	// from cache, silently un-guarding the exact defect (C-1) this test
	// exists to catch. Reading them here makes their content part of the
	// cache key too, since go test tracks os.ReadFile calls made by the
	// test binary itself (N-4) — verified: touching either file busts the
	// cache on the next `go test` run with no flags.
	if _, err := os.ReadFile(script); err != nil {
		t.Fatalf("reading %s to make it part of the test cache key: %v", script, err)
	}
	if _, err := os.ReadFile(fixture); err != nil {
		t.Fatalf("reading %s to make it part of the test cache key: %v", fixture, err)
	}

	const driver = `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("abbench", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
desc = m.load_ballast_fixture(sys.argv[2])["description"]
mock, tools = sys.argv[3], int(sys.argv[4])
print(json.dumps({
    "bob": m.ballast_servers(mock, 1, tools, desc)["ballast0"]["args"],
    "lp": m.ballast_lp_entries(mock, 1, tools, desc)[0]["args"],
}))
`
	out, err := exec.Command(py, "-c", driver, script, fixture, mockBin,
		strconv.Itoa(tools)).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("running Layer 2 to read its ballast args failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Fatalf("running Layer 2 to read its ballast args failed: %v", err)
	}

	var got struct {
		Bob []string `json:"bob"`
		LP  []string `json:"lp"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parsing Layer 2's ballast args: %v (output: %s)", err, out)
	}
	return got.Bob, got.LP
}

// mockPayload is the exact tools/list payload a client holds when it dials
// mockmcp with args — the quantity both layers are supposed to agree about.
func mockPayload(t *testing.T, mockBin string, args []string) []byte {
	t.Helper()
	c, err := Dial(mockBin, args...)
	if err != nil {
		t.Fatalf("Dial(%v): %v", args, err)
	}
	defer c.Close()
	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize(%v): %v", args, err)
	}
	raw, err := c.ToolsListRaw()
	if err != nil {
		t.Fatalf("ToolsListRaw(%v): %v", args, err)
	}
	return raw
}

// TestBallastWeightIsIdenticalAcrossLayers is the regression guard for review
// C-1, and it is the deliverable that finding asked for — not the one-line fix.
//
// abreport.py joins a Layer 1 residency record to a Layer 2 live record on
// `ballast_tools` ALONE. If the two layers' ballast tools weigh different
// amounts, a live session that carried (say) 15.6 KB of ballast is paired with
// a residency measurement of 67.7 KB and reported at the same x-axis position,
// and nothing downstream can detect it: both records agree about the tool
// COUNT, which is the only thing the join looks at.
//
// That is exactly what happened. Layer 1 passed --description; Layer 2, written
// three tasks later, did not, so its tools fell back to mockmcp's 46-character
// default — 156 B/tool against Layer 1's 677 B/tool, a 4.3x mislabelling of the
// single number this harness exists to produce. No per-task review could see
// it: one layer is Go, the other Python, and each was reviewed alone.
//
// So this test does not compare source code or constants. It runs BOTH layers'
// ballast definitions against the real mockmcp binary and compares the actual
// tools/list payloads byte for byte. It fails if the layers ever again disagree
// about ballast weight for ANY reason — a dropped flag, a different tool count,
// a re-inlined description, a new mockmcp option one layer adopts and the other
// does not.
func TestBallastWeightIsIdenticalAcrossLayers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds mockmcp; skipped in -short")
	}
	const toolsPerServer = 3
	mock := buildMockMCP(t)

	layer1 := BallastSpecs(mock, 1, toolsPerServer)[0].Args
	layer2Bob, layer2LP := layer2BallastArgs(t, mock, toolsPerServer)

	// Argument-level equality first: it localises a drift to the flag that
	// caused it, which a byte count alone cannot do.
	if !reflect.DeepEqual(layer1, layer2Bob) {
		t.Errorf("Layer 1 and Layer 2's agent-attached (native arm) ballast args differ:\n"+
			"  Layer 1 (tests/bench/e2e/ballast.go BallastSpecs): %q\n"+
			"  Layer 2 (scripts/abbench.py ballast_servers):      %q\n"+
			"Both must come from tests/bench/e2e/fixtures/ballast.json.", layer1, layer2Bob)
	}
	if !reflect.DeepEqual(layer1, layer2LP) {
		t.Errorf("Layer 1 and Layer 2's behind-the-proxy (router/lazy arm) ballast args differ:\n"+
			"  Layer 1 (BallastSpecs):                              %q\n"+
			"  Layer 2 (scripts/abbench.py ballast_lp_entries):     %q", layer1, layer2LP)
	}

	// Then the quantity that actually matters: the bytes a client holds.
	want := mockPayload(t, mock, layer1)
	for name, args := range map[string][]string{
		"Layer 2 native-arm ballast": layer2Bob,
		"Layer 2 proxy-arm ballast":  layer2LP,
	} {
		got := mockPayload(t, mock, args)
		if !bytes.Equal(got, want) {
			t.Errorf("%s carries a different tools/list payload than Layer 1's: "+
				"%d bytes vs %d (%.2fx). abreport.py joins the two layers on "+
				"ballast_tools alone, so this mislabels the breakeven curve's x-axis "+
				"by exactly that ratio and nothing downstream can notice.",
				name, len(got), len(want), float64(len(got))/float64(len(want)))
		}
	}

	// A live guard on the failure mode itself: mockmcp's default description
	// really is dramatically lighter, so "both layers pass --description" is
	// load-bearing, not decorative.
	bare := mockPayload(t, mock, []string{"--tools=" + strconv.Itoa(toolsPerServer)})
	if len(bare) >= len(want) {
		t.Fatalf("mockmcp's default description is no longer lighter than the shared "+
			"one (%d vs %d bytes) — this test's premise no longer holds and the "+
			"guard needs rethinking, not deleting", len(bare), len(want))
	}
}
