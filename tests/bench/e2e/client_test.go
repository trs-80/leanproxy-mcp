package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/internal/cachefile"
)

// buildMockMCP compiles the standalone mock MCP server and returns its path.
func buildMockMCP(t testing.TB) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockmcp")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/trs-80/leanproxy-mcp-bob/tests/bench/mockmcp/cmd")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build mockmcp: %v", err)
	}
	return bin
}

func TestClientToolsList(t *testing.T) {
	bin := buildMockMCP(t)

	c, err := Dial(bin, "--tools=3")
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

	got := strings.Count(string(raw), `"name"`)
	if got != 3 {
		t.Fatalf("expected 3 tools in payload, got %d: %s", got, raw)
	}
}

// TestDialIsolatesHomeDirectory guards against a real incident: running
// `go test ./tests/bench/e2e/` against the real leanproxy-mcp binary (see
// buildLeanproxy in arms_test.go) created ~/.config/leanproxy/toolcache/
// ballast0.json and ballast1.json in the operator's real home directory,
// because Dial never set cmd.Env and internal/cachefile.Dir resolves the
// cache root via os.UserHomeDir (i.e. $HOME). Task 4 spawns the proxy
// hundreds of times across a sweep; every spawn must be contained to a
// private HOME, or CI runners and developer machines pick up permanent,
// silently-stale cache files under a real home directory.
func TestDialIsolatesHomeDirectory(t *testing.T) {
	lp := buildLeanproxy(t)
	mock := buildMockMCP(t)
	specs := BallastSpecs(mock, 1, 3)

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine real home dir: %v", err)
	}
	cacheDir := filepath.Join(realHome, ".config", "leanproxy", "toolcache")
	before, _ := os.ReadDir(cacheDir) // nil/empty if the dir doesn't exist yet

	if _, err := Capture(ArmLazy, lp, specs, t.TempDir()); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	after, _ := os.ReadDir(cacheDir)
	beforeNames := make(map[string]bool, len(before))
	for _, e := range before {
		beforeNames[e.Name()] = true
	}
	for _, e := range after {
		if !beforeNames[e.Name()] {
			t.Fatalf("Capture wrote %s into the real cache dir %s; Dial must isolate the subprocess's HOME", e.Name(), cacheDir)
		}
	}
}

// statSnapshot is a lightweight fingerprint of a status file: whether it
// exists, and (on Unix) its inode. Inode, not mtime, is what actually detects
// a delete-and-recreate: a live leanproxy daemon on the developer's machine
// rewrites its status file in place every few seconds (cmd/server.go's 5s
// status ticker), which changes mtime but keeps the same inode. Only a
// delete+recreate — the exact shape of the CRITICAL-1 bug below — changes it.
type statSnapshot struct {
	exists bool
	ino    uint64
}

func snapshotStatusFile(path string) statSnapshot {
	info, err := os.Stat(path)
	if err != nil {
		return statSnapshot{}
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return statSnapshot{exists: true, ino: st.Ino}
	}
	// Non-Unix (no syscall.Stat_t): fall back to existence only.
	return statSnapshot{exists: true}
}

// TestSweepDoesNotTouchRealStatusFile guards a real incident found in review:
// pkg/statusfile.NewFileStatusStore resolved its config root via
// user.Current().HomeDir, which reads the OS passwd database via getpwuid and
// IGNORES $HOME — unlike internal/cachefile.Dir, which uses os.UserHomeDir
// for exactly this reason (see its doc comment). Dial's HOME isolation
// (TestDialIsolatesHomeDirectory above) only covers os.UserHomeDir
// consumers, so it never protected the status store: every router/lazy
// Capture spawned a real leanproxy process that wrote its PID and an empty
// server list to the OPERATOR'S real ~/.config/leanproxy/status/current.json
// and then DELETED it on shutdown (statusStore.RemoveFile(), wired in
// cmd/server.go), regardless of the isolated HOME the subprocess was given.
// A sweep spawns 18 proxies; the last one to exit destroyed the operator's
// live status file. Fixed in pkg/statusfile/file.go by switching every
// user.Current().HomeDir use to os.UserHomeDir(), matching cachefile.
//
// A plain "no new file appeared" check (as in TestDialIsolatesHomeDirectory)
// would NOT have caught this: current.json already exists for anyone running
// a real leanproxy daemon, so the bug overwrites and deletes an EXISTING
// entry rather than adding a new one. This test tracks that entry's inode
// specifically, which changes if it is deleted and recreated (as the bug
// did) but not if a live daemon merely rewrites it in place.
//
// CRITICAL-1's actual shape is CREATE-then-DELETE: the buggy store creates
// its own file at the real path, then deletes it on shutdown. On a machine
// with no pre-existing status file — every CI runner, and any developer
// machine with no live leanproxy daemon — the DELETE half is not exercised
// here, because there is nothing to delete. That gap is deliberately NOT
// closed by planting a file: this test must never write to, create a
// directory under, or delete anything at the real path. os.Stat is the only
// access to the real home it is allowed to make. Planting would make the
// guard participate in the very race it exists to detect, would leave a
// lying `{"pid":-1}` status file behind on an interrupted run (which
// `leanproxy status` would then report forever), and would collide with
// tests/e2e, which spawns the real binary without LEANPROXY_HOME and so
// creates and removes this same real current.json concurrently.
//
// The delete branch is instead covered deterministically and
// non-destructively, entirely inside a temp dir, by
// pkg/statusfile.TestStatusStoreHonorsLeanproxyHomeOverride
// (pkg/statusfile/file_test.go:563), which fails outright under the original
// user.Current().HomeDir implementation.
//
// Watching the real path is a backstop, not the detector. Measured: with the
// bug reinstated, a sweep creates the real current.json and each proxy's exit
// takes it away again, so a before/after pair straddling the whole sweep sees
// exists=false both times and this test passes while the bug fires. The
// detector is TestSpawnedProxyWritesItsStatusFileUnderTheIsolatedHome below,
// which asserts the positive half of the contract — LeanProxy's state lands in
// the isolated home — and therefore fails the moment a proxy resolves its
// state root anywhere else.
func TestSweepDoesNotTouchRealStatusFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries; skipped in -short")
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine real home dir: %v", err)
	}
	statusDir := filepath.Join(realHome, ".config", "leanproxy", "status")
	statusFile := filepath.Join(statusDir, "current.json")

	// Read-only: stat and nothing else. See the doc comment above — planting
	// here would write into the operator's real config root.
	before := snapshotStatusFile(statusFile)

	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	// A small sweep across both proxy arms (native never spawns leanproxy, so
	// it can't touch the status store) — enough spawns to reproduce the
	// pattern that broke this, without repeating the full residency sweep.
	for _, tools := range []int{2, 8} {
		specs := BallastSpecs(mock, 2, tools/2)
		for _, arm := range []Arm{ArmRouter, ArmLazy} {
			if _, err := Capture(arm, lp, specs, t.TempDir()); err != nil {
				t.Fatalf("capture arm=%s tools=%d: %v", arm, tools, err)
			}
		}
	}

	after := snapshotStatusFile(statusFile)
	if before.exists && !after.exists {
		t.Fatalf("sweep deleted the real status file %s", statusFile)
	}
	if before.exists && before.ino != 0 && before.ino != after.ino {
		t.Fatalf("sweep replaced the real status file %s: inode changed %d -> %d (deleted and recreated)",
			statusFile, before.ino, after.ino)
	}
	if !before.exists && after.exists {
		t.Fatalf("sweep created a status file at the real path %s; LeanProxy state must stay under %s",
			statusFile, cachefile.HomeEnv)
	}
}

// TestSpawnedProxyWritesItsStatusFileUnderTheIsolatedHome is the detector the
// real-path watch above cannot be. It asserts where LeanProxy's state DOES
// land rather than where it does not, so it fails deterministically — no
// polling, no timing window, no dependence on what the operator's home
// happens to contain.
//
// cmd/server.go builds its status store during startup (NewFileStatusStore
// then updateStdioServerStatusOnce) before it serves a single request, so a
// completed initialize handshake is proof the file has already been written.
// Reinstating the bug — resolving the store's root with os.UserHomeDir or
// user.Current().HomeDir instead of cachefile.HomeDir, which ignores
// LEANPROXY_HOME — leaves the isolated home empty and fails this test.
func TestSpawnedProxyWritesItsStatusFileUnderTheIsolatedHome(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries; skipped in -short")
	}

	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)

	cfg, err := WriteConfig(t.TempDir(), BallastSpecs(mock, 1, 2))
	if err != nil {
		t.Fatalf("write proxy config: %v", err)
	}

	c, err := Dial(lp, "server", "run", "--stdio", "--config", cfg)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()

	if err := c.Initialize(); err != nil {
		t.Fatalf("initialize proxy: %v", err)
	}

	statusFile := filepath.Join(c.StateDir(), ".config", "leanproxy", "status", "current.json")
	if _, err := os.Stat(statusFile); err != nil {
		t.Fatalf("proxy did not write its status file under the isolated %s=%s: stat %s: %v\n"+
			"LeanProxy resolved its state root somewhere else — which means it wrote into the operator's real home",
			cachefile.HomeEnv, c.StateDir(), statusFile, err)
	}
}

// TestDialIsolatesLeanproxyStateWithoutMovingTheRealHome is the containment
// rule this harness actually needs: isolate what LeanProxy writes for itself,
// and change nothing else about the environment it measures.
//
// Overriding HOME did both. Every upstream MCP server the proxy spawns
// inherits its environment, so an isolated HOME moved the home of servers the
// harness does not own. codebase-memory-mcp derives its cache directory from
// HOME and refuses to start when that disagrees with its already-running
// daemon, so `preflight` reported all three arms unable to reach any expected
// tool and refused a configuration whose live sweep runs under the operator's
// real HOME and would have worked. The preflight was gating on an environment
// it would never measure — and the same blind spot could as easily have
// produced a false PASS.
func TestDialIsolatesLeanproxyStateWithoutMovingTheRealHome(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine real home dir: %v", err)
	}

	c, err := Dial(buildMockMCP(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	env := map[string]string{}
	for _, kv := range c.cmd.Env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}

	if env["HOME"] != realHome {
		t.Fatalf("Dial moved HOME to %q; upstream servers must see the operator's real home %q",
			env["HOME"], realHome)
	}
	state := env[cachefile.HomeEnv]
	if state == "" {
		t.Fatalf("Dial set no %s; LeanProxy's own state would land in the operator's config root", cachefile.HomeEnv)
	}
	if state == realHome {
		t.Fatalf("%s points at the real home %q; it must be a private directory", cachefile.HomeEnv, state)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("stat %s=%q: %v", cachefile.HomeEnv, state, err)
	}
}
