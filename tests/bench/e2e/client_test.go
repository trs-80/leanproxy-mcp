package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// buildMockMCP compiles the standalone mock MCP server and returns its path.
func buildMockMCP(t testing.TB) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockmcp")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/mmornati/leanproxy-mcp/tests/bench/mockmcp/cmd")
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
// machine with no live leanproxy daemon — before.exists and after.exists are
// both false regardless of whether the fix is in place, so a bare
// before/after existence-and-inode check never exercises the delete branch
// and passes vacuously. To make this a real guard everywhere, plant a
// sentinel file first whenever nothing already exists, so there is always
// something the bug's delete step would have to destroy.
var sentinelStatusContent = []byte(`{"pid":-1,"sentinel":"TestSweepDoesNotTouchRealStatusFile"}`)

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

	before := snapshotStatusFile(statusFile)
	sentinelPlanted := false
	if !before.exists {
		if err := os.MkdirAll(statusDir, 0700); err != nil {
			t.Fatalf("prepare status dir %s: %v", statusDir, err)
		}
		if err := os.WriteFile(statusFile, sentinelStatusContent, 0600); err != nil {
			t.Fatalf("plant sentinel status file %s: %v", statusFile, err)
		}
		t.Cleanup(func() { os.Remove(statusFile) })
		sentinelPlanted = true
		before = snapshotStatusFile(statusFile)
		if !before.exists {
			t.Fatalf("planted sentinel status file %s but it does not exist immediately after writing it", statusFile)
		}
	}

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
	if !after.exists {
		t.Fatalf("sweep deleted the real status file %s", statusFile)
	}
	if before.ino != 0 && before.ino != after.ino {
		t.Fatalf("sweep replaced the real status file %s: inode changed %d -> %d (deleted and recreated)",
			statusFile, before.ino, after.ino)
	}
	if sentinelPlanted {
		// Nothing legitimate could have rewritten a file under this test's
		// own sentinel name in the seconds this test ran — unlike the
		// inode-only check above, which must tolerate a live daemon
		// periodically rewriting ITS OWN pre-existing file in place, a
		// planted sentinel can be checked for byte-identical content too.
		got, err := os.ReadFile(statusFile)
		if err != nil {
			t.Fatalf("read status file after sweep: %v", err)
		}
		if string(got) != string(sentinelStatusContent) {
			t.Fatalf("sweep modified the planted sentinel status file %s: got %q, want %q",
				statusFile, got, sentinelStatusContent)
		}
	}
}
