package e2e

import (
	"encoding/json"
	"fmt"
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
	cmd := exec.Command("go", "build", "-o", bin, "github.com/trs-80/leanproxy-mcp-bob")
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

// TestCaptureRejectsUnknownArm: Capture previously had no default case, so an
// unrecognized Arm silently fell through to router mode (the "arm != ArmNative"
// branch runs regardless of arm's actual value, and only ArmLazy is checked
// explicitly for the --lazy-tools flag). A typo'd or future Arm value should
// fail loudly instead of quietly measuring the wrong thing.
func TestCaptureRejectsUnknownArm(t *testing.T) {
	mock := buildMockMCP(t)
	lp := buildLeanproxy(t) // a valid binary, so any error must come from arm validation, not a bad path
	specs := BallastSpecs(mock, 1, 1)

	_, err := Capture(Arm("bogus"), lp, specs, t.TempDir())
	if err == nil {
		t.Fatal("expected an error for an unknown Arm, got nil")
	}
}

func toolNames(t *testing.T, payload []byte) []string {
	t.Helper()
	var parsed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("unmarshal tools/list payload: %v (payload: %s)", err, payload)
	}
	names := make([]string, len(parsed.Tools))
	for i, tool := range parsed.Tools {
		names[i] = tool.Name
	}
	return names
}

// TestCaptureRouterExposesWrapperTools guards against under-delivery: a
// smaller router payload currently passes TestCaptureRouterIsSmallerThanNative
// whether it's smaller because it's correctly minimal (3 wrappers) or because
// it's wrongly empty (a regression that returned zero tools would still be
// "smaller than native" and pass every size-only assertion). Assert the
// actual wrapper set by name.
func TestCaptureRouterExposesWrapperTools(t *testing.T) {
	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	specs := BallastSpecs(mock, 2, 5)

	payload, err := Capture(ArmRouter, lp, specs, t.TempDir())
	if err != nil {
		t.Fatalf("Capture router: %v", err)
	}

	names := toolNames(t, payload)
	want := map[string]bool{"list_tools": true, "invoke_tool": true, "search_tools": true}
	if len(names) != len(want) {
		t.Fatalf("expected %d router wrapper tools, got %d: %v", len(want), len(names), names)
	}
	for _, name := range names {
		if !want[name] {
			t.Errorf("unexpected router tool %q, want one of list_tools/invoke_tool/search_tools", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("router payload is missing wrapper tools: %v", want)
	}
}

// TestCaptureLazyExposesPrefixedStubs is the lazy-arm counterpart to
// TestCaptureRouterExposesWrapperTools: asserts the actual stub count and
// name shape (servername_toolname), not just relative payload size, so a
// regression returning half the stubs — still smaller than native, still
// larger than router — cannot pass silently.
func TestCaptureLazyExposesPrefixedStubs(t *testing.T) {
	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	const servers, toolsPerServer = 2, 5
	specs := BallastSpecs(mock, servers, toolsPerServer)

	payload, err := Capture(ArmLazy, lp, specs, t.TempDir())
	if err != nil {
		t.Fatalf("Capture lazy: %v", err)
	}

	names := toolNames(t, payload)
	if got, want := len(names), servers*toolsPerServer; got != want {
		t.Fatalf("expected %d lazy stubs (servers x toolsPerServer), got %d: %v", want, got, names)
	}

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
		ok := false
		for s := 0; s < servers; s++ {
			if strings.HasPrefix(name, fmt.Sprintf("ballast%d_", s)) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("lazy stub %q does not carry a serverName_ prefix", name)
		}
	}
	for s := 0; s < servers; s++ {
		for i := 0; i < toolsPerServer; i++ {
			want := fmt.Sprintf("ballast%d_tool_%d", s, i)
			if !seen[want] {
				t.Errorf("expected lazy stub %q, not present", want)
			}
		}
	}
}
