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
