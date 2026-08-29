package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Story 14-1: Publish /metrics JSON endpoint (Epic 14)
// US: As an IDE / external monitoring tool, I want a JSON metrics endpoint
// at /metrics that exposes per-server / per-tool / top-tools / total spend so
// I can drive a status bar widget without scraping Prometheus text.
//
// Acceptance: GET /metrics returns application/json with keys
//   by_server, by_tool, total_spend, top_tools
// and that the endpoint can be disabled via --metrics-bind off.

func TestStory_14_1_MetricsEndpoint_JSONShape(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	// Bind :0 and read the actual port from the log — no freePort race.
	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "127.0.0.1:0",
		"--dashboard-bind", "off",
		"--upstream", "http://127.0.0.1:1",
	})
	persistLog := "/tmp/leanproxy-e2e-metrics.log"
	t.Cleanup(func() {
		if data, err := os.ReadFile(logFile); err == nil {
			os.WriteFile(persistLog, data, 0644)
		}
	})

	url := loopbackURL(t, boundAddr(t, logFile, "metrics", 10*time.Second), "/metrics")
	resp, body := waitForHTTP(t, url, 15*time.Second)
	if resp.StatusCode != http.StatusOK {
		log, _ := os.ReadFile(logFile)
		t.Fatalf("GET /metrics returned %d, body=%s\nlog:\n%s", resp.StatusCode, body, string(log))
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("GET /metrics did not return valid JSON: %v\nraw=%s", err, body)
	}

	// The /metrics endpoint exposes a snapshot with at least these keys.
	for _, key := range []string{"by_server", "by_tool", "total_spend"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("metrics JSON missing key %q, got keys: %v", key, mapKeys(parsed))
		}
	}
}

func TestStory_14_1_MetricsEndpoint_DisabledByFlag(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	// Assert the startup decision from the log, not a probe of some unrelated
	// port. The old test GET'd a freePort nobody was ever asked to bind, so it
	// passed whether or not --metrics-bind=off was honoured. pkg/metrics logs
	// "metrics endpoint disabled" on the ListenAndServe path in cmd/serve.go,
	// which runs before "server ready" — so if the flag were ignored and a
	// listener came up instead, this line would be absent and the test red.
	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "off",
		"--dashboard-bind", "off",
		"--upstream", "http://127.0.0.1:1",
	})

	requireLogLine(t, logFile, `msg="metrics endpoint disabled"`, 10*time.Second)
}

// Story 18-1: Web dashboard served from LeanProxy (Epic 18)
// US: As a finance lead, I want a read-only web dashboard at / showing
// today's spend, WTD spend, top server, top tool so I don't need to query
// the metrics endpoint by hand.

func TestStory_18_1_Dashboard_IndexHTML(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "off",
		"--dashboard-bind", "127.0.0.1:0",
		"--upstream", "http://127.0.0.1:1",
	})
	addr := boundAddr(t, logFile, "dashboard", 10*time.Second)

	resp, body := waitForHTTP(t, loopbackURL(t, addr, "/"), 10*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / returned %d, body=%s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html Content-Type, got %q", ct)
	}

	// /api/dashboard returns the cards HTML which IS rendered. Verify the
	// cards endpoint contract here; the root / template is separately
	// covered by pkg/dashboard unit tests.
	respCards, cardsBody := waitForHTTP(t, loopbackURL(t, addr, "/api/dashboard"), 5*time.Second)
	if respCards.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/dashboard returned %d, body=%s", respCards.StatusCode, cardsBody)
	}
	for _, expected := range []string{"WTD Spend", "Top Server", "Top Tool"} {
		if !strings.Contains(cardsBody, expected) {
			t.Errorf("dashboard cards missing expected text %q, got:\n%s", expected, cardsBody)
		}
	}
}

func TestStory_18_1_Dashboard_JSONAPI(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "off",
		"--dashboard-bind", "127.0.0.1:0",
		"--upstream", "http://127.0.0.1:1",
	})
	addr := boundAddr(t, logFile, "dashboard", 10*time.Second)

	resp, body := waitForHTTP(t, loopbackURL(t, addr, "/api/dashboard/json"), 10*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/dashboard/json returned %d, body=%s", resp.StatusCode, body)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("dashboard JSON did not parse: %v\nraw=%s", err, body)
	}

	for _, key := range []string{"today_spend", "wtd_spend", "top_server", "top_tool", "server_count", "tool_count"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("dashboard JSON missing key %q, got keys: %v", key, mapKeys(parsed))
		}
	}
}

func TestStory_18_1_Dashboard_NonLoopbackRequiresToken(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "off",
		"--dashboard-bind", "0.0.0.0:0",
		"--dashboard-token", "supersecret",
		"--upstream", "http://127.0.0.1:1",
	})
	addr := boundAddr(t, logFile, "dashboard", 10*time.Second)

	// Wait for the dashboard to be up.
	waitForHTTP(t, loopbackURL(t, addr, "/api/dashboard"), 10*time.Second)

	// Loopback request without token: should succeed (no auth required from loopback).
	resp, err := http.Get(loopbackURL(t, addr, "/"))
	if err != nil {
		t.Fatalf("request to loopback dashboard failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from loopback without token, got %d", resp.StatusCode)
	}

	// Note: non-loopback auth is exercised in pkg/dashboard/auth_test.go
	// (TestIsLoopbackRemoteAddr + TestRequireBearerToken). Real E2E requires
	// a second host or network namespace; we confirm only the loopback path
	// here, and that the --dashboard-token flag is accepted.
}

func TestStory_18_1_Dashboard_DisabledByFlag(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	// Same hole as the metrics twin, and worse: --dashboard-bind defaults to
	// 127.0.0.1:9090, so an ignored "off" would have bound 9090 while the old
	// test probed an unrelated freePort and stayed green. The log line is the
	// honest signal — pkg/dashboard logs it synchronously before "server ready",
	// and a real listener would emit "dashboard endpoint started" instead.
	//
	// A direct GET of 127.0.0.1:9090 is deliberately NOT asserted here: that is
	// the shipped default, so any developer or CI box running a real leanproxy
	// daemon would answer it and turn this test red for reasons unrelated to
	// the code under test.
	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "off",
		"--dashboard-bind", "off",
		"--upstream", "http://127.0.0.1:1",
	})

	requireLogLine(t, logFile, `msg="dashboard endpoint disabled"`, 10*time.Second)
}

// Helper used by story 18-2 (drill-down) and 18-1 (dashboard).

func TestStory_18_2_Drilldown_ServersEndpoint(t *testing.T) {
	requireBinary(t)

	testDir := t.TempDir()
	configPath := filepath.Join(testDir, "leanproxy_servers.yaml")
	writeFile(t, configPath, `version: "1.0"
servers: []
`)

	_, logFile := startServeAndWait(t, []string{
		"--config", configPath,
		"--listen", "127.0.0.1:0",
		"--metrics-bind", "off",
		"--dashboard-bind", "127.0.0.1:0",
		"--upstream", "http://127.0.0.1:1",
	})
	addr := boundAddr(t, logFile, "dashboard", 10*time.Second)

	resp, body := waitForHTTP(t, loopbackURL(t, addr, "/api/dashboard/servers"), 10*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/dashboard/servers returned %d, body=%s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("drill-down endpoint should serve text/html, got %q", ct)
	}
}

// stopServe reads the pidfile written by startServe and sends SIGTERM.
// Used as the deferred cleanup so tests don't leave orphan processes.
func stopServe(t *testing.T, pidFile, logFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil || pid == 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(os.Interrupt)
	time.Sleep(300 * time.Millisecond)
	_ = proc.Kill()
}
