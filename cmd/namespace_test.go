package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/registry"
)

func TestNamespaceCmd_HelpRendersNamespaceHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "namespace")
}

func TestNamespaceListCmd_HelpRendersItsOwnHelp(t *testing.T) {
	requireHelpFor(t, "namespace", "list")
}

func TestNamespaceAddCmd_HelpRendersItsOwnHelp(t *testing.T) {
	requireHelpFor(t, "namespace", "add")
}

func TestNamespaceAssignCmd_HelpRendersItsOwnHelp(t *testing.T) {
	requireHelpFor(t, "namespace", "assign")
}

func TestNamespaceListCmd_ReportsNoneConfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "leanproxy.yaml")
	if err := os.WriteFile(configPath, []byte("namespaces: {}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := requireCLISucceeds(t, "namespace", "list", "--config", configPath)

	if !strings.Contains(out, "No namespaces configured") {
		t.Errorf("`namespace list` on an empty config should say so\ngot:\n%s", out)
	}
}

// TestNamespaceListCmd_ListsConfiguredNamespaces is the assertion the old test
// could not make: that the command reads the config it was handed. The
// predecessor pointed --config at an EMPTY namespaces map and asserted only
// err == nil, so it would have passed against a command that ignored --config
// entirely — which, since it never executed, it effectively did.
func TestNamespaceListCmd_ListsConfiguredNamespaces(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "leanproxy.yaml")
	config := `namespaces:
  engineering:
    description: Engineering team
    servers:
      - github
      - jira
`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := requireCLISucceeds(t, "namespace", "list", "--config", configPath)

	for _, want := range []string{"Configured namespaces:", "engineering", "Engineering team", "[2 servers]"} {
		if !strings.Contains(out, want) {
			t.Errorf("`namespace list` output missing %q\ngot:\n%s", want, out)
		}
	}
}

// TestNamespaceAddCmd_PrintsConfigToPasteAndWritesNothing pins what `namespace
// add` actually does: it is ADVISORY. runNamespaceAdd (namespace.go:153-184)
// prints the YAML the operator should paste into leanproxy.yaml and persists
// nothing at all.
//
// The predecessor asserted err == nil from namespaceAddCmd.Execute() under the
// name "AddNamespace", which reads as though a namespace was added. Nothing
// was, and nothing was ever going to be. This test states the real contract so
// that making the command persist becomes a deliberate, test-breaking change.
func TestNamespaceAddCmd_PrintsConfigToPasteAndWritesNothing(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "leanproxy.yaml")
	if err := os.WriteFile(configPath, []byte("namespaces: {}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	out := requireCLISucceeds(t, "namespace", "add", "engineering",
		"--servers=github,jira", "--description=Engineering team", "--config", configPath)

	for _, want := range []string{
		"Adding namespace 'engineering'",
		"  Servers: github,jira",
		"  Description: Engineering team",
		"Note: Namespace configuration should be added to leanproxy.yaml",
		"        - github",
		"        - jira",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`namespace add` output missing %q\ngot:\n%s", want, out)
		}
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("`namespace add` modified %s, but it is documented as advisory only\nbefore:\n%s\nafter:\n%s",
			configPath, before, after)
	}
}

// TestNamespaceAssignCmd_PrintsInstructionsAndWritesNothing is the same
// contract for `assign` (namespace.go:186-196).
func TestNamespaceAssignCmd_PrintsInstructionsAndWritesNothing(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "leanproxy.yaml")
	if err := os.WriteFile(configPath, []byte("namespaces: {}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	out := requireCLISucceeds(t, "namespace", "assign", "engineering", "github", "--config", configPath)

	if !strings.Contains(out, "Assigning server 'github' to namespace 'engineering'") {
		t.Errorf("`namespace assign` did not report the assignment\ngot:\n%s", out)
	}
	if !strings.Contains(out, "Note: This operation requires updating leanproxy.yaml") {
		t.Errorf("`namespace assign` must say the change is not persisted\ngot:\n%s", out)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("`namespace assign` modified %s, but it is documented as advisory only", configPath)
	}
}

// TestNamespaceAddCmd_RequiresExactlyOneName pins the Args validator at
// namespace.go:54. Nothing previously exercised any namespace argument
// contract, because no namespace test ever reached cobra's argument parsing.
func TestNamespaceAddCmd_RequiresExactlyOneName(t *testing.T) {
	out, err := runCLI(t, "namespace", "add")

	if err == nil {
		t.Fatalf("`namespace add` with no name must fail\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("error should explain the argument count, got: %v", err)
	}
}

func TestGetChildNames(t *testing.T) {
	children := map[string]*registry.Namespace{
		"child1": {Name: "child1"},
		"child2": {Name: "child2"},
	}

	result := getChildNames(children)
	if len(result) != 2 {
		t.Errorf("expected 2 children, got %d", len(result))
	}
}

func TestGetChildNames_Empty(t *testing.T) {
	children := map[string]*registry.Namespace{}

	result := getChildNames(children)
	if len(result) != 0 {
		t.Errorf("expected 0 children, got %d", len(result))
	}
}

func TestGetChildNames_NilMap(t *testing.T) {
	result := getChildNames(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 children for nil map, got %d", len(result))
	}
}
