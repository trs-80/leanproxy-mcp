package e2e

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mmornati/leanproxy-mcp/pkg/migrate"
)

func TestBallastSpecs(t *testing.T) {
	specs := BallastSpecs("/tmp/mockmcp", 3, 25)
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d", len(specs))
	}
	if specs[0].Name != "ballast0" {
		t.Errorf("expected ballast0, got %s", specs[0].Name)
	}
	if specs[2].Args[0] != "--tools=25" {
		t.Errorf("expected --tools=25, got %s", specs[2].Args[0])
	}
}

func TestBallastSpecsZero(t *testing.T) {
	if got := BallastSpecs("/tmp/mockmcp", 0, 25); len(got) != 0 {
		t.Fatalf("expected no specs at zero ballast, got %d", len(got))
	}
}

func TestWriteConfigIsLoadable(t *testing.T) {
	dir := t.TempDir()
	specs := BallastSpecs("/tmp/mockmcp", 2, 10)

	path, err := WriteConfig(dir, specs)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(raw)

	for _, want := range []string{`version: "1"`, "ballast0", "ballast1", "--tools=10", "transport: stdio", "enabled: true"} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}

	// The substring checks above only prove the text is present, not that
	// it parses. Load the file through the same production loader the
	// proxy uses (cmd/serve.go) to actually exercise loadability.
	cfg, err := migrate.LoadConfig(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig returned nil config")
	}
	if len(cfg.Servers) != len(specs) {
		t.Fatalf("expected %d servers, got %d", len(specs), len(cfg.Servers))
	}

	first := cfg.Servers[0]
	if first.Name != specs[0].Name {
		t.Errorf("expected server name %s, got %s", specs[0].Name, first.Name)
	}
	if first.Stdio == nil {
		t.Fatal("expected stdio config, got nil")
	}
	if first.Stdio.Command != specs[0].Command {
		t.Errorf("expected command %q, got %q", specs[0].Command, first.Stdio.Command)
	}
	if len(first.Stdio.Args) != len(specs[0].Args) || first.Stdio.Args[0] != specs[0].Args[0] {
		t.Errorf("expected args %v, got %v", specs[0].Args, first.Stdio.Args)
	}
}

// TestBallastToolIsRealisticWeight guards against the ballast schema silently
// degenerating back into the tiny stub that made TestCaptureLazySitsBetween-
// RouterAndNative fail: with a one-property inputSchema and a description
// under stubDescChars (pkg/mcp/discovery.go), lazy-mode compaction has
// nothing to strip and the "servername_" prefix alone makes lazy bigger than
// native. live-snapshot.json (tests/bench/fixtures/live-snapshot.json) puts
// real upstream tools (github/garmin/intervals) at 400 bytes/tool; a ballast
// tool must land in that neighborhood for the sweep to mean anything.
func TestBallastToolIsRealisticWeight(t *testing.T) {
	mock := buildMockMCP(t)
	specs := BallastSpecs(mock, 1, 1)

	c, err := Dial(specs[0].Command, specs[0].Args...)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	raw, err := c.ToolsListRaw()
	if err != nil {
		t.Fatalf("ToolsListRaw: %v", err)
	}

	const wantMin, wantMax = 350, 450
	if got := len(raw); got < wantMin || got > wantMax {
		t.Fatalf("single-tool tools/list payload is %d bytes, want %d-%d (realistic per-tool weight, see live-snapshot.json)", got, wantMin, wantMax)
	}
}

// TestWriteConfigCommandWithColonSpace is a regression test: a command path
// containing a colon-space sequence (e.g. "C:\ tools\mockmcp") used to be
// interpolated into the YAML as an unquoted plain scalar, which the real
// loader rejects with "mapping values are not allowed in this context".
// WriteConfig must quote Command the same way it already quotes Args.
func TestWriteConfigCommandWithColonSpace(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{
		Name:    "ballast0",
		Command: "/opt/weird: path/mockmcp",
		Args:    []string{"--tools=5"},
	}}

	path, err := WriteConfig(dir, specs)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	cfg, err := migrate.LoadConfig(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil || len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got config %+v", cfg)
	}
	if cfg.Servers[0].Stdio == nil {
		t.Fatal("expected stdio config, got nil")
	}
	if got := cfg.Servers[0].Stdio.Command; got != specs[0].Command {
		t.Errorf("expected command %q to round-trip, got %q", specs[0].Command, got)
	}
}
