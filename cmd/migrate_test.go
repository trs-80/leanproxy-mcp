package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateFlags_Defaults(t *testing.T) {
	resetAllCommandFlags(t)

	tests := []struct {
		name string
		flag string
	}{
		{"yes", "yes"},
		{"dry-run", "dry-run"},
		{"validate-only", "validate-only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := migrateCmd.Flags().GetBool(tt.flag)
			if err != nil {
				t.Fatalf("get flag %s: %v", tt.flag, err)
			}
			if got {
				t.Errorf("flag %s should default to false", tt.flag)
			}
		})
	}
}

func TestMigrateCmd_HelpRendersMigrateHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "migrate")
}

// plantDiscoverableMCPConfig writes the file the generic scanner reads
// (~/.config/mcp.json, pkg/migrate/generic.go:27) inside the isolated HOME
// runCLI installs, so `migrate` has something to find.
//
// Without this every migrate test takes the "No MCP configurations found"
// early return at migrate.go:46 and none of --dry-run, --validate-only or the
// confirmation gate is ever reached — which is what the previous tests in this
// file did, on top of never executing the command at all.
func plantDiscoverableMCPConfig(t *testing.T, home, serverName, command string) {
	t.Helper()

	dir := filepath.Join(home, ".config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("prepare ~/.config in isolated home: %v", err)
	}
	// The key is mcp_servers, not the mcpServers that Claude, VS Code and the
	// wider ecosystem use: genericConfig tags it `json:"mcp_servers"`
	// (pkg/migrate/generic.go:11), while claude.go:11 tags the same concept
	// `json:"mcpServers"`. Getting this wrong makes the scanner find nothing
	// and every assertion below fall through to the empty-scan path — which is
	// exactly how it failed the first time this test was written.
	config := `{"mcp_servers":{"` + serverName + `":{"command":"` + command + `","args":[]}}}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("plant discoverable mcp.json: %v", err)
	}
}

// isolatedHomeWithMCPConfig returns a temp dir standing in for HOME, holding
// one discoverable MCP server. Callers point HOME at it and then use
// runCLIPreservingHome, which keeps that HOME rather than replacing it.
func isolatedHomeWithMCPConfig(t *testing.T, serverName, command string) string {
	t.Helper()

	home := t.TempDir()
	plantDiscoverableMCPConfig(t, home, serverName, command)
	return home
}

// TestMigrateCmd_ReportsNothingToImportOnACleanMachine pins the early return
// at migrate.go:46-50. This is the path every previous test in this file
// silently took while claiming to test --dry-run and --validate-only.
func TestMigrateCmd_ReportsNothingToImportOnACleanMachine(t *testing.T) {
	out := requireCLISucceeds(t, "migrate", "--dry-run")

	if !strings.Contains(out, "No MCP configurations found on this system.") {
		t.Errorf("migrate on an isolated empty home should report nothing found\ngot:\n%s", out)
	}
}

// TestMigrateCmd_DryRunListsDiscoveredServersWithoutWriting is the real
// --dry-run test: a server is discoverable, migrate must list it, say it
// changed nothing, and leave the target file absent.
func TestMigrateCmd_DryRunListsDiscoveredServersWithoutWriting(t *testing.T) {
	home := isolatedHomeWithMCPConfig(t, "fixture-server", "echo")
	target := filepath.Join(t.TempDir(), "servers.yaml")
	t.Setenv("HOME", home)
	t.Setenv("LEANPROXY_CONFIG", target)

	out, err := runCLIPreservingHome(t, "migrate", "--dry-run")
	if err != nil {
		t.Fatalf("migrate --dry-run failed: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "fixture-server") {
		t.Errorf("--dry-run should list the discovered server\ngot:\n%s", out)
	}
	if !strings.Contains(out, "Dry-run mode: no changes were made.") {
		t.Errorf("--dry-run should say it changed nothing\ngot:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("--dry-run wrote the target config %s; it must not touch disk", target)
	}
}

// TestMigrateCmd_ValidateOnlyReportsValidationWithoutImporting pins the
// --validate-only block at migrate.go:80-101.
func TestMigrateCmd_ValidateOnlyReportsValidationWithoutImporting(t *testing.T) {
	home := isolatedHomeWithMCPConfig(t, "fixture-server", "echo")
	target := filepath.Join(t.TempDir(), "servers.yaml")
	t.Setenv("HOME", home)
	t.Setenv("LEANPROXY_CONFIG", target)

	out, err := runCLIPreservingHome(t, "migrate", "--validate-only")
	if err != nil {
		t.Fatalf("migrate --validate-only failed: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "--- Validation Mode ---") {
		t.Errorf("--validate-only should enter validation mode\ngot:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("--validate-only wrote the target config %s; validation must not import", target)
	}
}

// TestMigrateCmd_CancelsRatherThanImportingWithoutConfirmation pins the
// confirmation gate at migrate.go:120-127. runCLI points stdin at /dev/null,
// so the prompt reads EOF and must take the cancel path — never the import.
func TestMigrateCmd_CancelsRatherThanImportingWithoutConfirmation(t *testing.T) {
	home := isolatedHomeWithMCPConfig(t, "fixture-server", "echo")
	target := filepath.Join(t.TempDir(), "servers.yaml")
	t.Setenv("HOME", home)
	t.Setenv("LEANPROXY_CONFIG", target)

	out, err := runCLIPreservingHome(t, "migrate")
	if err != nil {
		t.Fatalf("migrate failed: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Import canceled.") {
		t.Errorf("an unconfirmed migrate must cancel\ngot:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a canceled migrate wrote %s", target)
	}
}

// TestGlobalDryRunFlag_IsParsedButNeverReachesDryRunEnabled documents a
// PRODUCTION BUG rather than hiding it.
//
// migrate.go:69 honors `migrateDryRun || DryRunEnabled`, and add.go:166,
// add.go:203 and serve.go:161 read DryRunEnabled too — but root.go:39
// registers the persistent flag as
//
//	RootCmd.PersistentFlags().BoolP("dry-run", "n", false, ...)
//
// with NO variable target, and nothing ever copies the parsed value into
// DryRunEnabled (root.go:29). So `--dry-run` is inert for every command that
// relies on the global rather than its own local flag.
//
// The predecessor of this test set DryRunEnabled = true by hand and asserted
// err == nil, which tested the package variable and never the flag, so the
// gap was invisible. This asserts the CURRENT broken behavior on purpose:
// when root.go is fixed to bind the flag (BoolVarP(&DryRunEnabled, ...) or a
// PersistentPreRun that copies it), this test fails and must be inverted —
// that failure is the point, and the message below says so.
//
// `version` is the vehicle rather than `migrate` because migrate declares its
// OWN local --dry-run (migrate.go:30) with no shorthand, which shadows the
// root's persistent one; `migrate -n` therefore fails with "unknown shorthand
// flag: 'n'". That shadowing is a second, separate defect: it means
// migrate.go:69's `|| DryRunEnabled` clause is unreachable from the command
// line by any spelling.
func TestGlobalDryRunFlag_IsParsedButNeverReachesDryRunEnabled(t *testing.T) {
	if DryRunEnabled {
		t.Fatal("test precondition: DryRunEnabled must start false")
	}

	out := requireCLISucceeds(t, "-n", "version")

	if !strings.Contains(out, "leanproxy-mcp version") {
		t.Fatalf("`-n version` did not run the version command\ngot:\n%s", out)
	}
	parsed, err := RootCmd.PersistentFlags().GetBool("dry-run")
	if err != nil {
		t.Fatalf("read parsed --dry-run: %v", err)
	}
	if !parsed {
		t.Fatal("cobra did not parse -n at all; this test can no longer detect the wiring gap")
	}
	if DryRunEnabled {
		t.Fatal("DryRunEnabled is now set by -n — the production bug at root.go:39 is FIXED. " +
			"Invert this test: assert DryRunEnabled is true, and rename it to " +
			"TestGlobalDryRunFlag_SetsDryRunEnabled.")
	}
}
