package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/compactor"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/migrate"
)

func TestRebuildCommand_UnknownServer(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")

	t.Setenv("LEANPROXY_CONFIG", configPath)

	cfg := `version: "1.0"
servers:
  - name: github
    transport: stdio
    stdio:
      command: echo
`
	if err := os.WriteFile(configPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := rebuildCmd
	args := []string{"nonexistent"}

	err := cmd.RunE(cmd, args)
	if err == nil {
		t.Error("expected error for unknown server")
	}
}

func TestRebuildCommand_NoArgsNoAllFlag(t *testing.T) {
	cmd := rebuildCmd
	args := []string{}

	err := cmd.RunE(cmd, args)
	if err == nil {
		t.Error("expected error when no args and no --all flag")
	}
}

func TestRebuildFlags_AllFlag(t *testing.T) {
	cmd := rebuildCmd

	all, err := cmd.Flags().GetBool("all")
	if err != nil {
		t.Fatalf("get --all flag: %v", err)
	}
	if all {
		t.Error("--all should default to false")
	}
}

func TestRebuildFlags_AllFlagSet(t *testing.T) {
	cmd := rebuildCmd

	if err := cmd.Flags().Set("all", "true"); err != nil {
		t.Fatalf("set --all flag: %v", err)
	}

	all, err := cmd.Flags().GetBool("all")
	if err != nil {
		t.Fatalf("get --all flag: %v", err)
	}
	if !all {
		t.Error("--all should be true after setting")
	}

	cmd.Flags().Set("all", "false")
}

func TestBuildRawManifest(t *testing.T) {
	cfg := &migrate.ServerConfig{
		Name:      "test-server",
		Transport: "stdio",
	}

	manifest := buildRawManifest("test-server", cfg)

	if manifest.Name != "test-server" {
		t.Errorf("manifest.Name = %v, want test-server", manifest.Name)
	}
	if len(manifest.Tools) == 0 {
		t.Error("manifest should have at least one tool")
	}
}

func TestRebuildCommand_DisabledServer(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")

	t.Setenv("LEANPROXY_CONFIG", configPath)

	disabled := false
	cfg := `version: "1.0"
servers:
  - name: github
    transport: stdio
    stdio:
      command: echo
    enabled: false
`
	if err := os.WriteFile(configPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_ = disabled

	cmd := rebuildCmd
	args := []string{"github"}

	err := cmd.RunE(cmd, args)
	if err == nil {
		t.Error("expected error for disabled server")
	}
}

func TestRebuildAllServers_NoServers(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")

	t.Setenv("LEANPROXY_CONFIG", configPath)

	cfg := `version: "1.0"
servers: []
`
	if err := os.WriteFile(configPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := rebuildAllServers(context.Background())
	if err == nil {
		t.Error("expected error when no servers")
	}
}

func TestRebuildAllServers_AllDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "servers.yaml")

	t.Setenv("LEANPROXY_CONFIG", configPath)

	disabled := false
	cfg := `version: "1.0"
servers:
  - name: github
    transport: stdio
    stdio:
      command: echo
    enabled: false
`
	if err := os.WriteFile(configPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_ = disabled

	err := rebuildAllServers(context.Background())
	if err == nil {
		t.Error("expected error when all servers disabled")
	}
}

func TestRebuildCommand_HelpRendersRebuildHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "compactor", "rebuild")
}

func TestCompactorCommand_HelpRendersCompactorHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "compactor")
}

// TestRebuildCommand_RequiresAServerNameOrAll pins the guard at
// compactor.go:59. This is the first test in this file to reach runRebuild at
// all: the two help tests above it never did, so nothing here exercised the
// command's own argument contract.
//
// rebuild uses RunE, so unlike most commands in this package its failures do
// reach the caller as an error — which is what makes this assertable.
func TestRebuildCommand_RequiresAServerNameOrAll(t *testing.T) {
	out, err := runCLI(t, "compactor", "rebuild")

	if err == nil {
		t.Fatalf("`compactor rebuild` with no server and no --all must fail\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "specify server name or use --all flag") {
		t.Errorf("error should name the missing argument, got: %v", err)
	}
}

// TestRebuildCommand_ReportsMissingServer proves the command reads the config
// it was given and rejects a name that is not in it.
func TestRebuildCommand_ReportsMissingServer(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "servers.yaml")
	if err := os.WriteFile(configPath, []byte("version: \"1.0\"\nservers: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("LEANPROXY_CONFIG", configPath)

	out, err := runCLI(t, "compactor", "rebuild", "no-such-server")

	if err == nil {
		t.Fatalf("rebuilding a server absent from the config must fail\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no servers configured") &&
		!strings.Contains(err.Error(), `"no-such-server" not found`) {
		t.Errorf("error should explain the server is not in the registry, got: %v", err)
	}
}

func TestJoinStrings(t *testing.T) {
	result := joinStrings([]string{"a", "b", "c"})
	expected := "a b c "
	if result != expected {
		t.Errorf("joinStrings() = %v, want %v", result, expected)
	}
}

func TestJoinStrings_Empty(t *testing.T) {
	result := joinStrings([]string{})
	expected := ""
	if result != expected {
		t.Errorf("joinStrings() = %v, want %v", result, expected)
	}
}

type mockLLMClient struct {
	distillFunc func(context.Context, compactor.RawManifest) (*compactor.DistilledManifest, error)
}

func (m *mockLLMClient) Distill(ctx context.Context, manifest compactor.RawManifest) (*compactor.DistilledManifest, error) {
	if m.distillFunc != nil {
		return m.distillFunc(ctx, manifest)
	}
	return nil, nil
}

type mockCacheForTest struct {
	getFunc        func(context.Context, string, string) (*compactor.DistilledManifest, error)
	setFunc        func(context.Context, string, *compactor.DistilledManifest) error
	invalidateFunc func(context.Context, string) error
}

func (m *mockCacheForTest) Get(ctx context.Context, serverName, originalHash string) (*compactor.DistilledManifest, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, serverName, originalHash)
	}
	return nil, nil
}

func (m *mockCacheForTest) Set(ctx context.Context, serverName string, manifest *compactor.DistilledManifest) error {
	if m.setFunc != nil {
		return m.setFunc(ctx, serverName, manifest)
	}
	return nil
}

func (m *mockCacheForTest) Invalidate(ctx context.Context, serverName string) error {
	if m.invalidateFunc != nil {
		return m.invalidateFunc(ctx, serverName)
	}
	return nil
}
