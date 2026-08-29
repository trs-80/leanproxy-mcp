package version

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The build stamps these variables through `-ldflags -X <module
// path>/internal/version.Version=...`, which names the package by its FULL
// module path. The Go linker silently ignores an -X flag whose symbol it
// cannot resolve: no warning, no build failure, just a released binary
// reporting `version dev / build date: unknown`.
//
// So a module path rename that misses one of these files produces a green
// build, a green test run, a successful GoReleaser run, and unversioned
// release artifacts — discovered only by running `leanproxy-mcp version` on a
// published binary. This test is the thing that notices instead.
func TestLdflagsTargetTheCurrentModulePath(t *testing.T) {
	root := repoRoot(t)

	module := modulePath(t, root)
	if module == "" {
		t.Fatal("no module line in go.mod")
	}
	want := module + "/internal/version"

	// Every -X target in the build configuration, wherever it appears.
	xTarget := regexp.MustCompile(`-X\s+(\S+?)\.(?:Version|Commit|BuildTime|BuiltBy)=`)

	for _, name := range []string{"Makefile", ".goreleaser.yml"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		matches := xTarget.FindAllStringSubmatch(string(data), -1)
		if len(matches) == 0 {
			t.Errorf("%s: no -X version stamping found; if it moved, update this test", name)
			continue
		}
		for _, m := range matches {
			if m[1] != want {
				t.Errorf("%s stamps %q but the module is %q — the linker will silently ignore this and ship an unversioned binary; expected %q",
					name, m[1], module, want)
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found in any parent directory")
		}
		dir = parent
	}
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}
