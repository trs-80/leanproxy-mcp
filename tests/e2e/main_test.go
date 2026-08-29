package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain builds the leanproxy-mcp binary for the e2e suite when
// LEANPROXY_E2E is set and the binary is not already present. Without the
// env var the suite keeps its historical behavior (skip when the binary is
// absent) so a plain `go test ./...` stays fast and hermetic; with it, a
// missing binary is a hard failure, so skips cannot hide in a green run.
func TestMain(m *testing.M) {
	if os.Getenv("LEANPROXY_E2E") != "" && !binaryAvailable() {
		wd, _ := os.Getwd()
		cmd := exec.Command("go", "build", "-o", filepath.Join(wd, "leanproxy-mcp"), "../..")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: failed to build leanproxy-mcp binary: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// requireBinary gates a test on the prebuilt binary. When LEANPROXY_E2E is
// set the binary is mandatory (fail, never skip); otherwise the test skips
// with a pointer at how to enable the suite.
func requireBinary(t *testing.T) {
	t.Helper()
	if binaryAvailable() {
		return
	}
	if os.Getenv("LEANPROXY_E2E") != "" {
		t.Fatal("LEANPROXY_E2E is set but the binary is missing from tests/e2e/ (TestMain build failed?)")
	}
	t.Skip("Binary not in tests/e2e/ — set LEANPROXY_E2E=1 to build it and run the e2e suite")
}

// homes memoizes the private state root handed to every child process a single
// test spawns. t.TempDir() returns a fresh directory on each call, so without
// memoization two runBinary calls in one test would see two different homes and
// a test that syncs a cache then asserts on it could never observe its own
// write.
var (
	homesMu sync.Mutex
	homes   = map[*testing.T]string{}
)

// testHome returns the private state root for t, creating it on first use.
// Tests that assert on a path the binary writes must resolve it under this
// directory, never under os.UserHomeDir().
func testHome(t *testing.T) string {
	t.Helper()
	homesMu.Lock()
	defer homesMu.Unlock()
	if h, ok := homes[t]; ok {
		return h
	}
	h := t.TempDir()
	homes[t] = h
	t.Cleanup(func() {
		homesMu.Lock()
		defer homesMu.Unlock()
		delete(homes, t)
	})
	return h
}

// testEnv is the environment every CLI child process runs under. Both roots
// have to move: LEANPROXY_HOME covers ~/.config/leanproxy (status file,
// toolstore, compactor) and HOME covers ~/.leanproxy (registry index,
// quarantine, semantic cache stats), which resolves through os.UserHomeDir and
// ignores LEANPROXY_HOME entirely. Without both, these tests read and mutate
// the operator's live LeanProxy state, and their results become a function of
// whatever that operator's daemon happened to record rather than of the code.
//
// Overriding HOME is safe here only because runBinary drives one-shot CLI
// subcommands that spawn no upstream MCP server. cachefile.HomeDir's doc
// comment explains why HOME must NOT be moved for a long-running `serve`: every
// upstream inherits the environment, so a fake HOME relocates their state too.
// startServe therefore sets LEANPROXY_HOME only.
func testEnv(t *testing.T) []string {
	t.Helper()
	home := testHome(t)
	return append(os.Environ(),
		"HOME="+home,
		"LEANPROXY_HOME="+home,
	)
}

func runBinary(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	wd, _ := os.Getwd()

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(filepath.Join(wd, "leanproxy-mcp"), args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = wd
	cmd.Env = testEnv(t)

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

func binaryAvailable() bool {
	wd, _ := os.Getwd()
	if _, err := os.Stat(filepath.Join(wd, "leanproxy-mcp")); err == nil {
		return true
	}
	return false
}

func TestCLI_HelpCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "--help")
	output := stdout + stderr

	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d. Output: %s", exitCode, output)
	}

	if !strings.Contains(output, "LeanProxy MCP") {
		t.Errorf("Expected help output, got: %s", output)
	}
}

func TestCLI_VersionCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "version")
	output := stdout + stderr

	if exitCode != 0 {
		t.Errorf("Expected exit code 0, got %d. Output: %s", exitCode, output)
	}

	if !strings.Contains(output, ".") && !strings.Contains(output, "v") {
		t.Errorf("Expected version output, got: %s", output)
	}
}

func TestCLI_InvalidCommand(t *testing.T) {
	requireBinary(t)

	_, stderr, exitCode := runBinary(t, "nonexistent-command")

	if exitCode == 0 {
		t.Errorf("Expected non-zero exit code for invalid command")
	}

	t.Logf("stderr: %s", stderr)
}

func TestServerList_OnMissingConfig_ReportsEmptyAndSucceeds(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "servers.yaml")
	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, exitCode := runBinary(t, "server", "list")

	if exitCode != 0 {
		t.Fatalf("server list against a nonexistent config %s: exit %d, want 0 (stdout=%q stderr=%q)",
			configPath, exitCode, stdout, stderr)
	}
	// Exact wording from cmd/server.go's empty-list branch; a change here is a
	// user-visible change and should break this test deliberately.
	if !strings.Contains(stdout, "No servers configured.") {
		t.Errorf("server list stdout = %q, want it to contain %q", stdout, "No servers configured.")
	}
}

func TestServerAdd_PersistsStdioServerToConfig(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "servers.yaml")
	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, exitCode := runBinary(t, "server", "add", "test-server", "echo", "hello", "--transport", "stdio")

	if exitCode != 0 {
		t.Fatalf("server add: exit %d, want 0 (stdout=%q stderr=%q)", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, `Server "test-server" added successfully`) {
		t.Errorf("server add stdout = %q, want it to confirm the add", stdout)
	}

	// The success message is not the contract; the persisted file is.
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("server add reported success but wrote no config at %s: %v", configPath, err)
	}
	for _, want := range []string{"name: test-server", "transport: stdio", "command: echo"} {
		if !strings.Contains(string(written), want) {
			t.Errorf("persisted config is missing %q, got:\n%s", want, written)
		}
	}
}

func TestServe_BasicStart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	requireBinary(t)

	t.Skip("Skipping serve test - requires running server")
}

func TestCacheHelp_SucceedsAndDocumentsTheCacheCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "cache", "--help")

	if exitCode != 0 {
		t.Fatalf("cache --help: exit %d, want 0 (stdout=%q stderr=%q)", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "cache") {
		t.Errorf("cache --help stdout = %q, want it to name the cache command", stdout)
	}
}

func TestStatusHelp_SucceedsAndDocumentsTheStatusCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "status", "--help")

	if exitCode != 0 {
		t.Fatalf("status --help: exit %d, want 0 (stdout=%q stderr=%q)", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "status") {
		t.Errorf("status --help stdout = %q, want it to name the status command", stdout)
	}
}

func TestConfigValidation_AcceptsValidTransportAndRejectsUnknownOne(t *testing.T) {
	requireBinary(t)

	tests := []struct {
		name string
		// config uses the nested `stdio:` form because that is the schema
		// migrate.ServerConfig actually declares. The flat `command:`/`args:`
		// spelling this table used to carry is silently dropped by yaml.v3 and
		// then rejected as "command is required for stdio transport", so the
		// row named "valid config" was in fact invalid.
		config     string
		wantExit   int
		wantStderr string
	}{
		{
			name: "valid stdio transport",
			config: `servers:
  - name: test
    transport: stdio
    stdio:
      command: echo
      args: [hello]`,
			wantExit: 0,
		},
		{
			name: "unknown transport",
			config: `servers:
  - name: test
    transport: invalid
    stdio:
      command: echo
      args: [hello]`,
			wantExit:   1,
			wantStderr: `invalid transport type "invalid"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDir := t.TempDir()
			configPath := filepath.Join(testDir, fmt.Sprintf("config-%s.yaml", tt.name))
			if err := os.WriteFile(configPath, []byte(tt.config), 0644); err != nil {
				t.Fatalf("Failed to write config: %v", err)
			}

			t.Setenv("LEANPROXY_CONFIG", configPath)

			stdout, stderr, exitCode := runBinary(t, "server", "list")

			if exitCode != tt.wantExit {
				t.Fatalf("server list on %s config: exit %d, want %d (stdout=%q stderr=%q)",
					tt.name, exitCode, tt.wantExit, stdout, stderr)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to name the offending field with %q", stderr, tt.wantStderr)
			}
		})
	}
}

// TestDryRunFlag_IsAcceptedByServerAdd only covers flag acceptance, which is
// strictly less than --dry-run's documented contract ("Preview actions without
// making changes"). The missing half is a CONFIRMED PRODUCTION BUG, not an
// oversight in this test:
//
//	$ leanproxy-mcp --dry-run server add dryrun-test echo test
//	Server "dryrun-test" added successfully      # exit 0
//	$ ls $LEANPROXY_CONFIG                        # the file IS written
//
// Cause: cmd/root.go registers the flag with PersistentFlags().BoolP(...) — no
// variable target — and nothing copies it into cmd.DryRunEnabled, so runServerAdd
// never sees it. The assertion that belongs here is:
//
//	if _, err := os.Stat(configPath); err == nil {
//		t.Fatal("--dry-run wrote the config file; it must preview only")
//	}
//
// It is deliberately NOT enabled yet because the fix is in production code
// (bind the flag with BoolVarP(&DryRunEnabled, ...) and have runServerAdd return
// before saveConfig). Enable it in the same change as that fix. Asserting the
// current write-through behavior instead would cement the bug, so this test
// asserts only what is unambiguously correct today.
func TestDryRunFlag_IsAcceptedByServerAdd(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "servers.yaml")
	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, exitCode := runBinary(t, "--dry-run", "server", "add", "dryrun-test", "echo", "test")

	if exitCode != 0 {
		t.Fatalf("--dry-run server add: exit %d, want 0 (stdout=%q stderr=%q)", exitCode, stdout, stderr)
	}
	if strings.Contains(strings.ToLower(stderr), "unknown flag") {
		t.Fatalf("--dry-run is not accepted as a global flag: stderr=%q", stderr)
	}
	if !strings.Contains(stdout, "dryrun-test") {
		t.Errorf("--dry-run server add stdout = %q, want it to name the server it would add", stdout)
	}
}

func TestJSONRPC_HealthEndpoint(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var health map[string]string
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if health["status"] != "ok" {
		t.Errorf("Expected status 'ok', got '%s'", health["status"])
	}
}

func TestJSONRPC_Initialize(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		if contentType != "application/json" {
			t.Errorf("Expected Content-Type application/json, got %s", contentType)
		}

		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("Failed to parse JSON-RPC request: %v", err)
		}

		if req["jsonrpc"] != "2.0" {
			t.Errorf("Expected JSONRPC 2.0, got %s", req["jsonrpc"])
		}

		if req["method"] == "initialize" {
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"result": map[string]interface{}{
					"protocolVersion": "1.0",
					"serverInfo": map[string]string{
						"name":    "LeanProxy-MCP",
						"version": "test",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	requestBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"id":      1,
	}
	body, _ := json.Marshal(requestBody)

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var rpcResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if rpcResp["jsonrpc"] != "2.0" {
		t.Errorf("Expected JSONRPC 2.0 in response, got %s", rpcResp["jsonrpc"])
	}
}

func TestJSONRPC_InvalidMethod(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		if req["method"] == "invalid_method" {
			errResp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"error": map[string]interface{}{
					"code":    -32601,
					"message": "Method not found",
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(errResp)
		}
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	requestBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "invalid_method",
		"id":      1,
	}
	body, _ := json.Marshal(requestBody)

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", resp.StatusCode)
	}
}

func TestJSONRPC_BatchRequest(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var requests []json.RawMessage
		if err := json.Unmarshal(body, &requests); err != nil {
			t.Logf("Not a batch request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			return
		}

		var responses []map[string]interface{}
		for _, reqRaw := range requests {
			var req map[string]interface{}
			json.Unmarshal(reqRaw, &req)
			responses = append(responses, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"result":  map[string]interface{}{},
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	batchRequest := []map[string]interface{}{
		{"jsonrpc": "2.0", "method": "tool1", "id": 1},
		{"jsonrpc": "2.0", "method": "tool2", "id": 2},
		{"jsonrpc": "2.0", "method": "tool3", "id": 3},
	}
	body, _ := json.Marshal(batchRequest)

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestErrorHandling(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32600,
				"message": "Invalid Request",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errResp)
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", resp.StatusCode)
	}

	var rpcResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("Failed to parse error response: %v", err)
	}

	if rpcResp["error"] == nil {
		t.Errorf("Expected error in response")
	}
}

func TestNewFeatures_NamespaceCommands(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "namespace", "--help")
	t.Logf("Namespace help: %s %s", stdout, stderr)

	if exitCode != 0 {
		t.Errorf("namespace --help should succeed, got exit code %d", exitCode)
	}

	if !strings.Contains(stdout, "namespace") {
		t.Errorf("Expected namespace command in help output")
	}

	stdout, stderr, _ = runBinary(t, "namespace", "list")
	t.Logf("Namespace list: %s %s", stdout, stderr)
}

func TestNewFeatures_CostCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "cost", "--help")
	t.Logf("Cost help: %s %s", stdout, stderr)

	if exitCode != 0 {
		t.Errorf("cost --help should succeed, got exit code %d", exitCode)
	}

	if !strings.Contains(stdout, "cost") && !strings.Contains(stdout, "token") {
		t.Errorf("Expected cost/token command in help output")
	}

	stdout, _, _ = runBinary(t, "cost")
	t.Logf("Cost output: %s", stdout)
}

func TestNewFeatures_SavingsCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "savings", "--help")
	t.Logf("Savings help: %s %s", stdout, stderr)

	if exitCode != 0 {
		t.Errorf("savings --help should succeed, got exit code %d", exitCode)
	}

	stdout, _, _ = runBinary(t, "savings")
	t.Logf("Savings output: %s", stdout)
}

func TestNewFeatures_FederationConfig(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "config.yaml")

	config := `server:
  port: 8080

federation:
  enabled: true
  peers:
    - name: "test-peer"
      url: "http://localhost:9999"
      auth_token: "test-token"

namespaces:
  engineering:
    description: "Engineering team"
    servers:
      - github
    children:
      frontend:
        servers:
          - storybook

optimization:
  lazy_loading:
    enabled: true
    stub_tokens: 54
    cache_ttl: 24h
`

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, exitCode := runBinary(t, "server", "list")
	t.Logf("Server list with federation config: %s %s", stdout, stderr)

	if exitCode != 0 {
		t.Errorf("server list should succeed with federation config, got exit code %d", exitCode)
	}
}

func TestNewFeatures_ServerHealthCommand(t *testing.T) {
	requireBinary(t)

	stdout, stderr, exitCode := runBinary(t, "server", "health", "--help")
	t.Logf("Server health help: %s %s", stdout, stderr)

	if exitCode != 0 {
		t.Errorf("server health --help should succeed, got exit code %d", exitCode)
	}
}

func TestConfig_ServerWithHTTPTransport(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "servers.yaml")

	config := `servers:
  - name: http-test
    transport: http
    http:
      url: http://localhost:9999/mcp
`

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, _ := runBinary(t, "server", "list")
	t.Logf("Server list with HTTP transport: %s %s", stdout, stderr)

	if !strings.Contains(stdout, "http-test") && !strings.Contains(stderr, "http-test") {
		t.Errorf("Expected http-test server in list output")
	}
}

func TestConfig_OptimizationSettings(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy.yaml")

	config := `server:
  port: 8080

optimization:
  lazy_loading:
    enabled: true
    stub_tokens: 54
    cache_ttl: 24h
    prewarm:
      - tool1
      - tool2

bouncer:
  enabled: true
  patterns:
    - name: custom-pattern
      type: regex
      pattern: "API_KEY=[A-Za-z0-9]+"
      replacement: "API_KEY=REDACTED"
`

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	wd, _ := os.Getwd()
	binaryPath := filepath.Join(wd, "leanproxy-mcp")

	os.Chdir(testDir)
	defer os.Chdir(wd)

	cmd := exec.Command(binaryPath, "bouncer", "list-patterns")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// This site builds its own command instead of using runBinary (it needs the
	// chdir above so the binary picks up ./leanproxy.yaml), so it has to opt
	// into the same isolation by hand.
	cmd.Env = testEnv(t)
	cmd.Run()

	output := stdout.String()
	t.Logf("Bouncer list-patterns: %s", output)

	if !strings.Contains(output, "aws-access-key") {
		t.Errorf("Expected aws-access-key pattern in list")
	}
}

func TestMCPConnection_Validation(t *testing.T) {
	requireBinary(t)

	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, ".config/leanproxy_servers.yaml")

	if _, err := os.Stat(configPath); err != nil {
		t.Skipf("No config at %s, skipping MCP connection test", configPath)
	}

	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, exitCode := runBinary(t, "server", "list")
	t.Logf("Server list from config: %s %s", stdout, stderr)

	if exitCode != 0 {
		t.Errorf("server list should succeed, got exit code %d", exitCode)
	}

	expectedServers := []string{"garmin", "Intervals.icu", "stitch", "github"}
	for _, server := range expectedServers {
		found := strings.Contains(stdout, server) || strings.Contains(stderr, server)
		if !found {
			t.Logf("Note: Server '%s' may not be in list output", server)
		}
	}
}

func TestMCPConnection_StdioServers(t *testing.T) {
	requireBinary(t)

	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, ".config/leanproxy_servers.yaml")

	if _, err := os.Stat(configPath); err != nil {
		t.Skipf("No config at %s, skipping", configPath)
	}

	t.Setenv("LEANPROXY_CONFIG", configPath)

	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{"garmin", 30 * time.Second},
		{"Intervals.icu", 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, exitCode := runBinary(t, "server", "health", tt.name, "--timeout", tt.timeout.String())
			t.Logf("Health check for %s: exit=%d, stdout=%s, stderr=%s", tt.name, exitCode, stdout, stderr)

			if exitCode != 0 && exitCode != 1 {
				t.Errorf("server health %s should not crash, got exit code %d", tt.name, exitCode)
			}
		})
	}
}

// TestMCPConnection_HTTPEndpoints was removed rather than repaired. It skipped
// entirely unless the developer's personal ~/.config/leanproxy_servers.yaml
// existed, and when it did run its subtests ran `server list` with no arguments
// and only t.Logf'd the output — the `server` loop variable never reached the
// command, so both subtests issued the identical call and asserted nothing.
// TestConfig_ServerWithHTTPTransport above already covers listing an
// http-transport server, from a hermetic t.TempDir() fixture. Any further
// HTTP-endpoint coverage belongs on a self-written fixture too, never on the
// operator's real home config.

func TestConfig_LoadFromHome(t *testing.T) {
	requireBinary(t)

	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, ".config/leanproxy_servers.yaml")

	if _, err := os.Stat(configPath); err != nil {
		t.Skipf("No config at %s", configPath)
	}

	t.Setenv("LEANPROXY_CONFIG", configPath)

	stdout, stderr, exitCode := runBinary(t, "server", "list")
	t.Logf("Load from home config: exit=%d, %s %s", exitCode, stdout, stderr)

	if exitCode != 0 {
		t.Errorf("Should load config from home directory, got exit code %d", exitCode)
	}
}

func TestServerRun_StdioMode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	requireBinary(t)

	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, ".config/leanproxy_servers.yaml")

	if _, err := os.Stat(configPath); err != nil {
		t.Skipf("No config at %s", configPath)
	}

	testDir := t.TempDir()
	outputPath := filepath.Join(testDir, "output.txt")

	file, err := os.Create(outputPath)
	if err != nil {
		t.Fatalf("Failed to create output file: %v", err)
	}
	defer file.Close()

	// NOTE: "./leanproxy-mcp" is resolved against cmd.Dir (testDir), which never
	// contains the binary, so cmd.Start() always fails and this test early-returns
	// without asserting anything. It is left that way deliberately: making the
	// path absolute would activate a test that loads the operator's personal
	// ~/.config/leanproxy_servers.yaml and spawns their real upstream MCP
	// servers, which is neither hermetic nor deterministic. Rewrite it against a
	// t.TempDir() fixture with a stub upstream (see integration_test.go's python
	// script) rather than un-breaking the path.
	//
	// LEANPROXY_HOME is set anyway so that if it ever does start, it cannot
	// overwrite the operator's live status file. HOME is left real because this
	// command spawns upstreams — cachefile.HomeDir's doc comment explains why.
	cmd := exec.Command("./leanproxy-mcp", "server", "run", "--stdio")
	cmd.Stdout = file
	cmd.Stderr = file
	cmd.Dir = testDir
	cmd.Env = append(os.Environ(),
		"LEANPROXY_CONFIG="+configPath,
		"LEANPROXY_HOME="+t.TempDir(),
	)

	err = cmd.Start()
	if err != nil {
		t.Logf("server run --stdio may require running MCP server, err: %v", err)
		return
	}

	time.Sleep(2 * time.Second)
	cmd.Process.Kill()
	cmd.Wait()

	content, _ := os.ReadFile(outputPath)
	t.Logf("server run output: %s", string(content))
}
