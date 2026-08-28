package e2e

import (
	"os"
	"strings"
	"testing"
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
}
