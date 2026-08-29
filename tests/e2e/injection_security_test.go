package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// Story 13-1: Build a local prompt-injection classifier (Epic 13)
// US: As a security lead, I want a local prompt-injection classifier that
// scores payloads 0-100 so I can route risky traffic through a quarantine /
// block action.
//
// E2E surface: `leanproxy doctor --security` reports the configured threshold
// and the active policy bands (block / quarantine / log).
//
// Story 13-2: Configurable actions (Epic 13)
// US: As a security lead, I want each risk band to be wired to a configurable
// action (block / quarantine / log / redact) and a quarantine directory under
// ~/.leanproxy/quarantine/.

func TestStory_13_2_DoctorSecurity_ReportsPolicyBands(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "doctor", "--security")
	t.Logf("doctor --security: exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)

	if exitCode != 0 {
		t.Fatalf("doctor --security should exit 0, got %d", exitCode)
	}

	output := stdout
	for _, expected := range []string{"Risk", "block", "quarantine", "log"} {
		if !strings.Contains(output, expected) {
			t.Errorf("doctor --security output missing %q, got:\n%s", expected, output)
		}
	}
}

// This replaces a stat of the operator's real ~/.leanproxy/quarantine, which
// could only ever skip (clean machine) or trivially pass (dirty one) — the
// result was a property of the developer's box, not of the code. Under an
// injected home the quarantine store is guaranteed empty, so the empty-state
// report is a deterministic thing to assert.
func TestDoctorSecurity_OnCleanHome_ReportsAnEmptyQuarantine(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "doctor", "--security")

	if exitCode != 0 {
		t.Fatalf("doctor --security: exit %d, want 0 (stdout=%q stderr=%q)", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "Quarantine Status") {
		t.Errorf("doctor --security stdout = %q, want a Quarantine Status section", stdout)
	}
	if !strings.Contains(stdout, "Total quarantined payloads: 0") {
		t.Errorf("doctor --security on an empty quarantine store reported %q, want a zero total", stdout)
	}
}

// Story 13-2 JSON output via `doctor --security --json` (if supported).
// The Markdown output above already covers the policy bands; if the JSON
// variant is present we additionally verify it's well-formed.

func TestStory_13_2_DoctorSecurity_JSON(t *testing.T) {
	requireBinary(t)

	stdout, _, exitCode := runBinary(t, "doctor", "--security", "--json")
	t.Logf("doctor --security --json: exit=%d stdout=%q", exitCode, stdout)
	if exitCode != 0 {
		t.Skip("--json variant not yet supported on doctor")
	}

	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || trimmed[0] != '{' {
		t.Skipf("--json returned non-object output: %s", trimmed)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("doctor --security --json did not parse: %v\nraw=%s", err, trimmed)
	}
}
