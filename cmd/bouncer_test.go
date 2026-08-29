package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/bouncer"
)

func TestBouncerCmd_HelpRendersBouncerHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "bouncer")
}

func TestValidatePatternsCmd_HelpRendersItsOwnHelp(t *testing.T) {
	requireHelpFor(t, "bouncer", "validate-patterns")
}

func TestListPatternsCmd_HelpRendersItsOwnHelp(t *testing.T) {
	requireHelpFor(t, "bouncer", "list-patterns")
}

// TestBouncerListPatterns_PrintsEveryBuiltInPattern asserts the command emits
// one line per built-in pattern, checked against the same source the command
// reads. Its predecessor asserted err == nil from listPatternsCmd.Execute(),
// which never reached the Run — so the command could have printed nothing, or
// silently lost every pattern, and the test still passed.
//
// The count assertion is what makes this bite: a substring check for a single
// well-known pattern name would survive the whole list being truncated.
func TestBouncerListPatterns_PrintsEveryBuiltInPattern(t *testing.T) {
	patterns := bouncer.GetBuiltInPatterns()
	if len(patterns) == 0 {
		t.Fatal("no built-in patterns to assert against; bouncer.GetBuiltInPatterns returned empty")
	}

	out := requireCLISucceeds(t, "bouncer", "list-patterns")

	if !strings.Contains(out, "# Built-in Patterns") {
		t.Errorf("list-patterns is missing its header\ngot:\n%s", out)
	}
	for _, p := range patterns {
		want := "  - " + p.Name + ": " + p.Description
		if !strings.Contains(out, want) {
			t.Errorf("list-patterns omitted pattern %q\nwant line: %q\ngot:\n%s", p.Name, want, out)
		}
	}
	if got := strings.Count(out, "\n  - "); got != len(patterns) {
		t.Errorf("list-patterns printed %d pattern lines, want %d (one per built-in)", got, len(patterns))
	}
}

// TestBouncerValidatePatterns_ReportsCountsForAConfigItWasGiven runs against a
// config this test writes, so the counts are known rather than whatever the
// developer's leanproxy.yaml happens to hold.
//
// The command os.Exit(1)s on a load or compile failure (bouncer.go:29, :41),
// which would take the whole test binary down — so the config must be valid.
// That fragility is worth noting but is production behavior, not a test bug.
func TestBouncerValidatePatterns_ReportsCountsForAConfigItWasGiven(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "leanproxy.yaml")
	config := `version: "1.0"
servers: []
bouncer:
  enabled: true
  custom_patterns:
    - name: test-token
      pattern: "tk-[0-9a-f]{8}"
      description: a fixture pattern
`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write bouncer config: %v", err)
	}

	out := requireCLISucceeds(t, "bouncer", "validate-patterns", "--config", configPath)

	builtIn := len(bouncer.GetBuiltInPatterns())
	want := "custom: 1, built-in: " + itoa(builtIn) + ")"
	if !strings.Contains(out, want) {
		t.Errorf("validate-patterns did not report the counts for the given config\nwant suffix: %q\ngot:\n%s", want, out)
	}
	if strings.Contains(out, "secret redaction is OFF") {
		t.Errorf("config sets bouncer.enabled: true, so the disabled warning must not appear\ngot:\n%s", out)
	}
}

// TestBouncerValidatePatterns_WarnsWhenRedactionIsDisabled pins the safety
// warning at bouncer.go:36. Losing it would silently ship a proxy that does no
// redaction while reporting a healthy pattern count.
func TestBouncerValidatePatterns_WarnsWhenRedactionIsDisabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "leanproxy.yaml")
	config := `version: "1.0"
servers: []
bouncer:
  enabled: false
`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write bouncer config: %v", err)
	}

	out := requireCLISucceeds(t, "bouncer", "validate-patterns", "--config", configPath)

	if !strings.Contains(out, "Warning: bouncer.enabled is false; secret redaction is OFF") {
		t.Errorf("validate-patterns must warn when redaction is disabled\ngot:\n%s", out)
	}
}

func TestBouncerCmd_ConfigFlagIsReadable(t *testing.T) {
	resetAllCommandFlags(t)

	if err := bouncerCmd.PersistentFlags().Set("config", "/tmp/test.yaml"); err != nil {
		t.Fatalf("set config flag: %v", err)
	}

	got, err := bouncerCmd.PersistentFlags().GetString("config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if got != "/tmp/test.yaml" {
		t.Errorf("config = %v, want /tmp/test.yaml", got)
	}
}

// itoa keeps the assertion above readable without pulling strconv into a file
// that needs it exactly once.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
