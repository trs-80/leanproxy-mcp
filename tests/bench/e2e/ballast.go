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

// BallastToolDescription is the description given to every ballast tool.
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
// conservative of the two, so its description length is the target here:
// 610 characters of real prose (not padding).
//
// This description is well past stubDescChars (160, pkg/mcp/discovery.go),
// so --lazy-tools truncation has real prose to cut. The flip point was
// measured directly (not just computed from stubDescChars+ellipsis+prefix):
// at <=160 description characters, truncateDescription is a no-op and lazy
// mode is strictly larger than native at every ballast size (the "servername_"
// prefix is pure overhead with nothing to offset it — this is the bug the
// previous version of this constant had). Between 160 and 165 characters the
// ordering flips; at 165+ characters lazy is smaller than native and stays
// smaller as descriptions grow further, since native's cost grows linearly
// with description length while lazy's stays capped at stubDescChars.
//
// Even at 610 characters this constant cannot reach 1449 bytes/tool, because
// mockmcp's inputSchema stays a single string-typed property
// (tests/bench/mockmcp/server.go), deliberately left trivial so compactSchema
// has nothing to compact — see the task-3 report for the resulting bias and
// its measured net direction.
const BallastToolDescription = "Searches the configured backend for items matching the given query and returns matching records with their metadata, pagination cursor, and total count. Use this when you need to look up items by keyword, tag, or free-text search rather than fetching a known ID directly. Supports filtering by status, date range, owner, and category; results are sorted by relevance unless a sort field is specified explicitly in the request parameters. Pass a smaller page_size for interactive use and a larger one for bulk export; the default page_size is 50 and the maximum is 500."

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
