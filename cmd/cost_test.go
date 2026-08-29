package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/reporter"
)

func TestCostCmd_Flags(t *testing.T) {
	tests := []struct {
		name string
		flag string
		set  string
		get  interface{}
	}{
		{"by-tool", "by-tool", "true", true},
		{"by-server", "by-server", "true", true},
		{"json", "json", "true", true},
		{"reset", "reset", "true", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := costCmd.Flags().Set(tt.flag, tt.set); err != nil {
				t.Fatalf("set flag %s: %v", tt.flag, err)
			}

			switch v := tt.get.(type) {
			case bool:
				got, err := costCmd.Flags().GetBool(tt.flag)
				if err != nil {
					t.Fatalf("get flag %s: %v", tt.flag, err)
				}
				if got != v {
					t.Errorf("flag %s = %v, want %v", tt.flag, got, v)
				}
			}
		})
	}
}

func TestCostCmd_HelpRendersCostHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "cost")
}

// TestCostCmd_ResetConfirmsCountersWereReset asserts on the confirmation the
// command prints (cost.go:37). The predecessor asserted err == nil from
// costCmd.Execute(), which never reached runCost.
func TestCostCmd_ResetConfirmsCountersWereReset(t *testing.T) {
	out := requireCLISucceeds(t, "cost", "--reset")

	if !strings.Contains(out, "Cost counters reset") {
		t.Errorf("`cost --reset` did not confirm the reset\ngot:\n%s", out)
	}
}

// TestCostCmd_JsonEmitsParseableJSON is the assertion that makes --json worth
// testing: that the output parses. Checking only that the command did not
// error would still pass if it printed the human table instead.
func TestCostCmd_JsonEmitsParseableJSON(t *testing.T) {
	out := requireCLISucceeds(t, "cost", "--json")

	trimmed := strings.TrimSpace(out)
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("`cost --json` did not emit valid JSON: %v\ngot:\n%s", err, out)
	}
	if _, ok := parsed.(map[string]any); !ok {
		t.Errorf("`cost --json` should emit a JSON object, got %T\noutput:\n%s", parsed, out)
	}
}

func TestGlobalCostTracker(t *testing.T) {
	tracker := reporter.GlobalCostTracker()
	if tracker == nil {
		t.Error("expected non-nil tracker")
	}
}
