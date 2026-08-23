package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// helper.go — utilities shared across story-specific *_test.go files.

func writeSimpleConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "leanproxy_servers.yaml")
	content := `version: "1.0"
servers:
  - name: echo
    transport: stdio
    enabled: true
    stdio:
      command: /bin/echo
      args: ["hello"]
      env: []
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

// freePort asks the OS for a free TCP port. Only suitable for negative tests
// that probe a port nothing should be listening on; positive tests must bind
// ":0" and recover the real address via boundAddr, because a port returned
// here can be re-taken by anyone between Close and the server's bind.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// writeFile is a tiny helper to materialize arbitrary files in a temp dir.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// startServe launches the binary as a background `serve` process, captures
// the PID into pidFile, and redirects stdout/stderr to logFile. The caller is
// responsible for invoking stopServe via t.Cleanup / defer.
func startServe(t *testing.T, args []string, pidFile, logFile string) error {
	t.Helper()
	wd, _ := os.Getwd()
	binaryPath := filepath.Join(wd, "leanproxy-mcp")
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("binary not found at %s", binaryPath)
	}

	logFh, err := os.Create(logFile)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}

	fullArgs := append([]string{"serve"}, args...)
	cmd := exec.Command(binaryPath, fullArgs...)
	cmd.Stdout = logFh
	cmd.Stderr = logFh
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "LEANPROXY_CONFIG="+findFirstArg(args, "--config"))

	if err := cmd.Start(); err != nil {
		logFh.Close()
		return fmt.Errorf("failed to start serve: %w", err)
	}

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		logFh.Close()
		return fmt.Errorf("failed to write pidfile: %w", err)
	}

	go func() {
		_ = cmd.Wait()
		logFh.Close()
	}()

	return nil
}

// runBinaryWithTimeout runs the binary with a hard timeout. If the timeout
// elapses before the process exits, the process is killed. Used for tests that
// need to assert CLI flag acceptance (e.g. serve --cache-strategy=X) without
// waiting for the serve process to start its long-running HTTP listeners.
func runBinaryWithTimeout(args []string, timeout time.Duration) (string, string, int) {
	wd, _ := os.Getwd()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, filepath.Join(wd, "leanproxy-mcp"), args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = wd

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return stdout.String(), stderr.String(), exitCode
}

// findFirstArg returns the value following --flag in args, or "" if absent.
func findFirstArg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// waitForServeReady polls until the serve process has logged its
// startup-complete line ("server ready") or has exited, whichever comes
// first. It replaces fixed startup sleeps: a crash fails immediately with the
// captured log, and a healthy start returns as soon as the server is up.
func waitForServeReady(t *testing.T, pidFile, logFile string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		logData, _ := os.ReadFile(logFile)
		if bytes.Contains(logData, []byte("server ready")) {
			return
		}
		if !pidAlive(t, pidFile) {
			t.Fatalf("serve exited before becoming ready. log:\n%s", logData)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for serve to become ready. log:\n%s", timeout, logData)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startServeAndWait starts a serve process with args, registers cleanup, and
// blocks until the server logs readiness (or fails the test with the log if
// it crashes). It returns the pidfile and logfile paths for assertions.
func startServeAndWait(t *testing.T, args []string) (pidFile, logFile string) {
	t.Helper()
	runDir := t.TempDir()
	pidFile = filepath.Join(runDir, "leanproxy.pid")
	logFile = filepath.Join(runDir, "leanproxy.log")
	if err := startServe(t, args, pidFile, logFile); err != nil {
		t.Fatalf("failed to start serve: %v", err)
	}
	t.Cleanup(func() { stopServe(t, pidFile, logFile) })
	waitForServeReady(t, pidFile, logFile, 15*time.Second)
	return pidFile, logFile
}

// boundAddr extracts the address a component actually bound, from its
// startup log line (e.g. `msg="metrics endpoint started" bind=127.0.0.1:52341`).
// Tests pass an explicit ":0" bind and read the address back, which removes
// the freePort allocate-close-rebind race entirely.
func boundAddr(t *testing.T, logFile, component string, timeout time.Duration) string {
	t.Helper()
	re := regexp.MustCompile(`msg="` + component + ` endpoint started" bind=(\S+)`)
	deadline := time.Now().Add(timeout)
	for {
		logData, _ := os.ReadFile(logFile)
		if m := re.FindSubmatch(logData); m != nil {
			return string(m[1])
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s endpoint address in log:\n%s", component, logData)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// loopbackURL converts a bound address (possibly on a wildcard interface)
// into a URL reachable via loopback.
func loopbackURL(t *testing.T, addr, path string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("unparseable bound address %q: %v", addr, err)
	}
	return "http://127.0.0.1:" + port + path
}

// waitForHTTP polls url until it returns 2xx or timeout elapses, returning
// the response and body. This is more resilient than a single GET because
// serve takes a moment to bind its dashboard/metrics listener.
func waitForHTTP(t *testing.T, url string, timeout time.Duration) (*http.Response, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return resp, string(body)
			}
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s after %s: %v", url, timeout, lastErr)
	return nil, ""
}
