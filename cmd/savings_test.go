package cmd

import (
	"strings"
	"testing"
)

func TestSavingsCmd_Flags(t *testing.T) {
	tests := []struct {
		name string
		flag string
		set  string
		get  interface{}
	}{
		{"reset", "reset", "true", true},
		{"server", "server", "testserver", "testserver"},
		{"json", "json", "true", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := savingsCmd.Flags().Set(tt.flag, tt.set); err != nil {
				t.Fatalf("set flag %s: %v", tt.flag, err)
			}

			switch v := tt.get.(type) {
			case bool:
				got, err := savingsCmd.Flags().GetBool(tt.flag)
				if err != nil {
					t.Fatalf("get flag %s: %v", tt.flag, err)
				}
				if got != v {
					t.Errorf("flag %s = %v, want %v", tt.flag, got, v)
				}
			case string:
				got, err := savingsCmd.Flags().GetString(tt.flag)
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

func TestSavingsCmd_HelpRendersSavingsHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "savings")
}

// TestSavingsCmd_ResetConfirmsCountersWereReset asserts the command reported
// the reset it performed (savings.go:36). Its predecessor asserted err == nil
// from savingsCmd.Execute(), which never reached runSavings — so `--reset`
// could have been a no-op, or panicked on a nil tracker, and the test passed.
func TestSavingsCmd_ResetConfirmsCountersWereReset(t *testing.T) {
	out := requireCLISucceeds(t, "savings", "--reset")

	if !strings.Contains(out, "Savings counters reset") {
		t.Errorf("`savings --reset` did not confirm the reset\ngot:\n%s", out)
	}
}

// TestSavingsCmd_ReportsSummaryWhenNoFlagsGiven pins the default rendering:
// the bare command prints the cumulative summary, not JSON and not a
// per-server breakdown.
func TestSavingsCmd_ReportsSummaryWhenNoFlagsGiven(t *testing.T) {
	out := requireCLISucceeds(t, "savings")

	for _, want := range []string{
		"=== Token Savings Summary ===",
		"Total Original Tokens:",
		"Total Optimized Tokens:",
		"Total Saved Tokens:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`savings` summary missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestGlobalSavingsTracker(t *testing.T) {
	if globalSavingsTracker == nil {
		t.Error("expected non-nil tracker")
	}
}

func TestDisplayCumulativeSavings(t *testing.T) {
	globalSavingsTracker.Reset()
	displayCumulativeSavings()
}

func TestDisplayServerSavings_NotFound(t *testing.T) {
	globalSavingsTracker.Reset()
	displayServerSavings("nonexistent")
}

func TestDisplayServerSavings(t *testing.T) {
	globalSavingsTracker.Reset()
	displayServerSavings("")
}
