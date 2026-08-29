package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/trs-80/leanproxy-mcp-bob/internal/cachefile"
)

// This file is the single way a test in this package may execute the CLI.
//
// It exists because 36 tests across ten files did this instead:
//
//	cmd := cacheCmd
//	cmd.SetArgs([]string{"--location"})
//	err := cmd.Execute()          // err is ALWAYS nil
//
// cobra's ExecuteC opens with "regardless of what command execute is called
// on, run on Root only" (command.go:1090) and then reads args from the ROOT's
// c.args, which no test ever set. With RootCmd.args nil and the binary named
// cmd.test rather than cobra.test, cobra falls back to os.Args[1:] — the
// go-test binary's own -test.* flags. stripFlags drops those during Find
// before ParseFlags, so no flag error is raised; RootCmd has no Run/RunE, so
// execute returns flag.ErrHelp, which ExecuteC turns into (cmd, nil) after
// printing the root help. Every one of those tests asserted err == nil against
// a value that could not be anything else, while the command under test never
// ran. runCLI removes the possibility of writing that test again.

// runCLI executes the real CLI exactly as a user's shell would: through the
// root command, with the arguments the caller names, capturing everything the
// process prints.
//
// Output capture goes through os.Stdout, not cmd.OutOrStdout(), because most
// of this CLI prints with bare fmt.Printf — showCacheLocation (cache.go:131)
// is typical, and only runCacheStats writes through the command's writer.
// Asserting on a bytes.Buffer passed to cmd.SetOut would therefore see nothing
// and pass vacuously, which is the very defect this harness exists to end.
//
// Callers must not pass a command that blocks (serve, server run --stdio,
// status --watch): runCLI waits for it to return.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	isolateCLIState(t)
	return runCLIPreservingHome(t, args...)
}

// runCLIPreservingHome is runCLI for the tests that must control HOME
// themselves — the migrate suite plants a discoverable ~/.config/mcp.json so
// the scanner has something to find, which runCLI's fresh TempDir would throw
// away.
//
// It refuses to run against the operator's real home. Getting that wrong would
// point `migrate` at the developer's actual MCP configuration and, without the
// stdin redirect below, offer to import it.
func runCLIPreservingHome(t *testing.T, args ...string) (string, error) {
	t.Helper()

	requireIsolatedHome(t)
	t.Setenv(cachefile.HomeEnv, t.TempDir())
	resetAllCommandFlags(t)

	RootCmd.SetArgs(args)
	t.Cleanup(func() { RootCmd.SetArgs(nil) })

	return captureProcessOutput(t, RootCmd.Execute)
}

// operatorHome is the real HOME, captured at package initialisation before any
// test can call t.Setenv. os.UserHomeDir READS $HOME, so it cannot serve as the
// reference once a test has overridden it — it would compare HOME to itself and
// the guard below would never fire.
var operatorHome = os.Getenv("HOME")

// requireIsolatedHome fails the test unless HOME has been moved off the
// operator's real home directory.
func requireIsolatedHome(t *testing.T) {
	t.Helper()

	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME is unset; set it to a temp dir before calling runCLIPreservingHome")
	}
	if operatorHome != "" && home == operatorHome {
		t.Fatalf("HOME is still the operator's real home (%s); a test that scans the filesystem must isolate it first", home)
	}
}

// isolateCLIState points every root this CLI resolves state under at a
// per-test temp dir. Once these commands genuinely execute they read and write
// real files — `savings --reset`, `cost --reset`, `namespace add` and `migrate`
// all persist — so without this the suite would start mutating the operator's
// own ~/.config/leanproxy and ~/.leanproxy on every run.
//
// LEANPROXY_CONFIG is deliberately NOT set here: several tests write a fixture
// config and set it themselves, and userConfigPath (server.go) prefers it over
// the home fallback. Isolating HOME covers the tests that do not.
func isolateCLIState(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv(cachefile.HomeEnv, dir)
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".config"), 0o700); err != nil {
		t.Fatalf("prepare isolated config dir: %v", err)
	}
}

// resetAllCommandFlags returns every flag on every command to its default and
// clears Changed.
//
// Clearing Changed is the part that matters and the part that is easy to miss.
// cacheCmd declares MarkFlagsMutuallyExclusive("semantic","clear","list",
// "search","location") (cache.go:52). Tests that poke flags directly —
// TestCacheCmd_Flags sets all seven — leave Changed set on the shared
// package-level command, so the next test that actually executes trips
// "if any flags in the group [...] are set none of the others can be" and
// fails for a reason that has nothing to do with what it was testing. Go runs
// tests in source order within a file and files in name order, so which test
// this hits depends on where you add one.
func resetAllCommandFlags(t *testing.T) {
	t.Helper()

	var reset func(c *cobra.Command)
	reset = func(c *cobra.Command) {
		clear := func(f *pflag.Flag) {
			if err := f.Value.Set(f.DefValue); err != nil {
				t.Fatalf("reset flag --%s on %q to %q: %v", f.Name, c.Name(), f.DefValue, err)
			}
			f.Changed = false
		}
		c.Flags().VisitAll(clear)
		c.PersistentFlags().VisitAll(clear)
		for _, sub := range c.Commands() {
			reset(sub)
		}
	}
	reset(RootCmd)
}

// captureProcessOutput redirects os.Stdout and os.Stderr to a pipe for the
// duration of fn and returns everything written to either, plus fn's error.
// The reader runs in its own goroutine because a command that outprints the
// pipe buffer would otherwise block forever on the write.
func captureProcessOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create capture pipe: %v", err)
	}

	// Stdin is redirected to /dev/null so a command that prompts cannot hang
	// the suite. `migrate` without --yes asks "Import to ...? [y/N]" and reads
	// with fmt.Scanln (migrate.go:121-123); an empty stdin gives it EOF, so it
	// takes the documented cancel path every time instead of depending on
	// whether the test binary happened to inherit a terminal.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s for stdin: %v", os.DevNull, err)
	}

	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin = devNull
	os.Stdout, os.Stderr = w, w
	RootCmd.SetIn(devNull)
	RootCmd.SetOut(w)
	RootCmd.SetErr(w)

	collected := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		collected <- buf.String()
	}()

	// Restore on the way out even if fn panics, so one failing test cannot
	// leave every later test in the package writing into a closed pipe.
	defer func() {
		os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr
		RootCmd.SetIn(nil)
		RootCmd.SetOut(nil)
		RootCmd.SetErr(nil)
		_ = devNull.Close()
		_ = r.Close()
	}()

	runErr := fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close capture pipe: %v", err)
	}
	return <-collected, runErr
}

// requireCLISucceeds runs the CLI and fails the test unless it exits without
// error, quoting the captured output — which is the only way to tell WHY a
// cobra command failed, since the message goes to the writer, not the error.
func requireCLISucceeds(t *testing.T, args ...string) string {
	t.Helper()

	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("%v failed: %v\noutput:\n%s", args, err, out)
	}
	return out
}

// requireHelpFor asserts that `<path...> --help` renders the help for that
// command and not, as every one of these tests used to, the root command's.
//
// The usage line is the discriminator: cobra renders "Usage:\n  leanproxy-mcp
// <full command path>", so root help and subcommand help differ there and
// almost nowhere else a substring match would notice.
func requireHelpFor(t *testing.T, path ...string) {
	t.Helper()

	out := requireCLISucceeds(t, append(append([]string{}, path...), "--help")...)

	want := "leanproxy-mcp"
	for _, p := range path {
		want += " " + p
	}
	if !contains(out, want) {
		t.Errorf("`%v --help` did not render help for that command\nwant usage line containing: %q\ngot:\n%s", path, want, out)
	}
}

// contains is strings.Contains under a name that reads as an assertion at the
// call sites above.
func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
