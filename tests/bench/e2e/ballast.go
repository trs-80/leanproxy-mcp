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
// Its length (not padding — a real sentence, since --lazy-tools truncates
// prose rather than counting it) is chosen so a ballast tool's tools/list
// entry lands near 400 bytes, matching what live-snapshot.json
// (tests/bench/fixtures/live-snapshot.json) measures for real upstream tools:
// github 16400B/41 tools, garmin 40000B/100, intervals 4000B/10 — all exactly
// 400 bytes/tool. At 308 characters this description exceeds stubDescChars
// (160, pkg/mcp/discovery.go), so --lazy-tools truncation actually has prose
// to cut; a shorter description no-ops truncation and understates lazy mode's
// benefit (as the original 46-char DescriptionBase did — see
// TestBallastToolIsRealisticWeight).
const BallastToolDescription = "Searches the configured backend for items matching the given query and returns matching records with their metadata, pagination cursor, and total count. Supports filtering by status, date range, and owner; results are sorted by relevance unless a sort field is specified explicitly in the request parameters."

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
