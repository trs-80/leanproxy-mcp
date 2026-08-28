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
