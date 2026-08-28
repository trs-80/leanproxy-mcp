package e2e

import (
	_ "embed"
	"encoding/json"
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

// ballastFixtureJSON is the shared ballast definition both layers of the A/B
// harness read. It lives in a data file rather than in this source file so
// that Layer 2 (scripts/abbench.py) consumes the SAME bytes rather than a
// hand-copied duplicate. The final review's C-1 is exactly what happens when
// they drift: Layer 2 omitted --description entirely and its ballast tools
// weighed 156 B each against Layer 1's 677 B, while abreport.py joined the two
// layers on ballast_tools alone — mislabelling the breakeven curve's x-axis by
// 4.3x with nothing downstream able to notice.
//
//go:embed fixtures/ballast.json
var ballastFixtureJSON []byte

// BallastFixture is the parsed fixtures/ballast.json. Only the fields both
// layers actually consume or assert on are modelled; the rest of the file is
// provenance prose for humans.
type BallastFixture struct {
	Description      string `json:"description"`
	DescriptionChars int    `json:"description_chars"`
	Measured         struct {
		LazyNativeFlipChars   int `json:"lazy_native_flip_chars"`
		TruncationStartsChars int `json:"truncation_starts_chars"`
	} `json:"measured"`
}

// Ballast is the fixture as loaded at package init. A fixture whose own
// recorded description_chars disagrees with the description it ships is a
// false measurement claim of exactly the kind this harness exists to avoid,
// so it panics rather than being quietly believed.
var Ballast = mustLoadBallastFixture()

func mustLoadBallastFixture() BallastFixture {
	var f BallastFixture
	if err := json.Unmarshal(ballastFixtureJSON, &f); err != nil {
		panic(fmt.Sprintf("tests/bench/e2e/fixtures/ballast.json is not valid JSON: %v", err))
	}
	if got := len(f.Description); got != f.DescriptionChars {
		panic(fmt.Sprintf(
			"fixtures/ballast.json: description is %d characters but description_chars says %d "+
				"— fix one of them; a recorded measurement that disagrees with the thing it "+
				"measures is the exact defect this fixture exists to prevent", got, f.DescriptionChars))
	}
	return f
}

// BallastToolDescription is the description given to every ballast tool, in
// BOTH layers — Layer 1 through this variable, Layer 2 through
// scripts/abbench.py's load_ballast_fixture(), reading the same file.
//
// live-snapshot.json is NOT a measurement — its own "source" field says
// "seeded-from-docs-index-md", docs/benchmark-results.md calls its numbers a
// placeholder, and its schema_bytes is exactly tool_count*400 for all three
// servers, i.e. invented. Do not cite it as evidence for a byte target.
//
// The real numbers come from leanproxy's own persisted tool caches for
// servers this repo actually proxies: ~/.config/leanproxy/toolcache/
// codebase-memory.json (15 tools) and context7.json (2 tools). Measured
// directly from those files: codebase-memory averages 1449 bytes/tool with a
// median description of 610 characters; context7 averages 2240 bytes/tool
// with a median description of 1218 characters. codebase-memory is the more
// conservative of the two, so its 610-character median was the TARGET. The
// prose actually written to hit it measures 568 characters — that is the
// shipped, measured length, and it is what both layers carry. (An earlier
// version of this comment claimed the literal was 610 characters. It never
// was; see review M-1.)
//
// 568 characters is well past stubDescChars (160, pkg/mcp/discovery.go), so
// --lazy-tools truncation has real prose to cut. The flip point was measured
// directly (not computed from stubDescChars+ellipsis+prefix) by sweeping
// description length and capturing real tools/list payload bytes at two
// shapes, 1 server x 25 tools and 2 servers x 50 tools, which agree exactly:
//
//   - at <= 160 characters truncateDescription is a no-op and lazy is
//     strictly larger than native at every ballast size (the "servername_"
//     prefix is pure overhead with nothing to offset it — the bug the first
//     version of this constant had);
//   - truncation first shortens the payload at 161 characters
//     (word-boundary snapping, pkg/mcp/format.go);
//   - lazy is still larger than native from 161 through 167, and first
//     becomes strictly smaller at 168 characters. (An earlier version of
//     this comment said "between 160 and 165"; the final review said 167.
//     Both are wrong: 167 is the last NON-flipped length. See review M-2.)
//
// Past 168 lazy stays smaller as descriptions grow, since native's cost grows
// linearly with description length while lazy's stays capped at
// stubDescChars.
//
// Even at 568 characters this cannot reach 1449 bytes/tool, because mockmcp's
// inputSchema stays a single string-typed property
// (tests/bench/mockmcp/server.go), deliberately left trivial so compactSchema
// has nothing to compact — see the task-3 report for the resulting bias and
// its measured net direction.
var BallastToolDescription = Ballast.Description

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
			Args: []string{
				fmt.Sprintf("--tools=%d", toolsPerServer),
				"--description=" + BallastToolDescription,
			},
		})
	}
	return specs
}

// WriteConfig writes a leanproxy_servers.yaml covering specs into dir and
// returns its path. The shape matches tests/e2e/helper_test.go:writeSimpleConfig,
// except command is quoted (%q, like args already were) so a command path
// containing a colon-space sequence still parses as valid YAML.
func WriteConfig(dir string, specs []Spec) (string, error) {
	var b strings.Builder
	b.WriteString("version: \"1\"\nservers:\n")
	for _, s := range specs {
		b.WriteString(fmt.Sprintf("  - name: %s\n", s.Name))
		b.WriteString("    transport: stdio\n")
		b.WriteString("    enabled: true\n")
		b.WriteString("    stdio:\n")
		b.WriteString(fmt.Sprintf("      command: %q\n", s.Command))
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
