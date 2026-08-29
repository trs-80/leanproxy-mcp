package cmd

import (
	"runtime"
	"strings"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/internal/version"
)

func TestVersionCmd_HelpRendersVersionHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "version")
}

// TestVersionCmd_PrintsVersionBuildAndPlatform runs the command the way a user
// does and asserts on what it actually printed. The predecessor called
// versionCmd.Execute() (which never reached runVersion) and then asserted
// err == nil, so it passed whatever the command printed — including nothing.
func TestVersionCmd_PrintsVersionBuildAndPlatform(t *testing.T) {
	out := requireCLISucceeds(t, "version")

	v := version.Get()
	for _, want := range []string{
		"leanproxy-mcp version " + v.Version,
		"build date: " + v.BuildTime,
		"platform: " + v.Platform,
		"go: " + runtime.Version(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`version` output missing %q\ngot:\n%s", want, out)
		}
	}
}
