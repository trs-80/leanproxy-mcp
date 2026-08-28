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
