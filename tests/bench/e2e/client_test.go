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

// TestDialIsolatesHomeDirectory guards against a real incident: running
// `go test ./tests/bench/e2e/` against the real leanproxy-mcp binary (see
// buildLeanproxy in arms_test.go) created ~/.config/leanproxy/toolcache/
// ballast0.json and ballast1.json in the operator's real home directory,
// because Dial never set cmd.Env and internal/cachefile.Dir resolves the
// cache root via os.UserHomeDir (i.e. $HOME). Task 4 spawns the proxy
// hundreds of times across a sweep; every spawn must be contained to a
// private HOME, or CI runners and developer machines pick up permanent,
// silently-stale cache files under a real home directory.
func TestDialIsolatesHomeDirectory(t *testing.T) {
	lp := buildLeanproxy(t)
	mock := buildMockMCP(t)
	specs := BallastSpecs(mock, 1, 3)

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine real home dir: %v", err)
	}
	cacheDir := filepath.Join(realHome, ".config", "leanproxy", "toolcache")
	before, _ := os.ReadDir(cacheDir) // nil/empty if the dir doesn't exist yet

	if _, err := Capture(ArmLazy, lp, specs, t.TempDir()); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	after, _ := os.ReadDir(cacheDir)
	beforeNames := make(map[string]bool, len(before))
	for _, e := range before {
		beforeNames[e.Name()] = true
	}
	for _, e := range after {
		if !beforeNames[e.Name()] {
			t.Fatalf("Capture wrote %s into the real cache dir %s; Dial must isolate the subprocess's HOME", e.Name(), cacheDir)
		}
	}
}
