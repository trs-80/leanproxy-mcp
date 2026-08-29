package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/trs-80/leanproxy-mcp-bob/internal/cachefile"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/cache"
)

func resetCacheStatsFlags(t *testing.T) {
	t.Helper()
	cacheStatsFlags.jsonOut = false
	cacheStatsFlags.model = ""
	if err := cacheStatsCmd.Flags().Set("json", "false"); err != nil {
		t.Fatalf("reset --json flag: %v", err)
	}
	if err := cacheStatsCmd.Flags().Set("model", ""); err != nil {
		t.Fatalf("reset --model flag: %v", err)
	}
}

func TestCacheCmd_Flags(t *testing.T) {
	tests := []struct {
		name   string
		flag   string
		set    string
		isBool bool
	}{
		{"list", "list", "true", true},
		{"server", "server", "testserver", false},
		{"search", "search", "testtool", false},
		{"json", "json", "true", true},
		{"clear", "clear", "true", true},
		{"location", "location", "true", true},
		{"semantic", "semantic", "true", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := cacheCmd.Flags().Set(tt.flag, tt.set); err != nil {
				t.Fatalf("set flag %s: %v", tt.flag, err)
			}

			if tt.isBool {
				got, err := cacheCmd.Flags().GetBool(tt.flag)
				if err != nil {
					t.Fatalf("get flag %s: %v", tt.flag, err)
				}
				if !got {
					t.Errorf("flag %s = %v, want true", tt.flag, got)
				}
			} else {
				got, err := cacheCmd.Flags().GetString(tt.flag)
				if err != nil {
					t.Fatalf("get flag %s: %v", tt.flag, err)
				}
				if got != tt.set {
					t.Errorf("flag %s = %v, want %v", tt.flag, got, tt.set)
				}
			}
		})
	}
}

func TestCacheCmd_HelpRendersCacheHelpNotRootHelp(t *testing.T) {
	requireHelpFor(t, "cache")
}

// TestCacheCmd_ListReportsAnEmptyCache runs against the isolated LEANPROXY_HOME
// runCLI installs, so "no cached tool data" is a fact this test established
// rather than an accident of the developer's machine.
//
// Its predecessor asserted err == nil from cacheCmd.Execute(). That is doubly
// vacuous here: the command never ran, AND cacheCmd uses Run (cache.go:18)
// rather than RunE, with runCache swallowing failures by printing them
// (cache.go:137, :143) — so even routed correctly, Execute returns nil whether
// the cache is readable or not. Only the output can distinguish those.
func TestCacheCmd_ListReportsAnEmptyCache(t *testing.T) {
	out := requireCLISucceeds(t, "cache", "--list")

	if !strings.Contains(out, "No cached tool data found") {
		t.Errorf("`cache --list` against an isolated empty cache should say so\ngot:\n%s", out)
	}
}

// TestCacheCmd_LocationPrintsTheIsolatedCacheDir is the assertion the old test
// could not make: that the path printed is the one LEANPROXY_HOME selects.
// A command that ignored the env var — which is exactly the class of bug that
// had the e2e suite writing into the operator's real home — fails here.
func TestCacheCmd_LocationPrintsTheIsolatedCacheDir(t *testing.T) {
	out := requireCLISucceeds(t, "cache", "--location")

	if !strings.Contains(out, "Tool cache location: ") {
		t.Fatalf("`cache --location` did not print a location\ngot:\n%s", out)
	}
	home := os.Getenv(cachefile.HomeEnv)
	if home == "" {
		t.Fatal("runCLI did not set an isolated " + cachefile.HomeEnv)
	}
	if !strings.Contains(out, home) {
		t.Errorf("`cache --location` printed a path outside the isolated %s=%s\ngot:\n%s",
			cachefile.HomeEnv, home, out)
	}
}

// TestCacheCmd_RejectsMutuallyExclusiveFlags pins the pairing declared at
// cache.go:52-53. It is also the canary for flag-state leakage: if some other
// test leaves Changed set on cacheCmd and resetAllCommandFlags stops clearing
// it, the tests above start failing with this error instead of their own.
func TestCacheCmd_RejectsMutuallyExclusiveFlags(t *testing.T) {
	out, err := runCLI(t, "cache", "--list", "--location")

	if err == nil {
		t.Fatalf("--list and --location are mutually exclusive and must be rejected\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "none of the others can be") {
		t.Errorf("error should name the mutual-exclusion group, got: %v", err)
	}
}

func TestCacheStatsCmd_HelpOutput(t *testing.T) {
	resetCacheStatsFlags(t)
	cache.GlobalCacheStatsTracker().Reset()

	var buf bytes.Buffer
	cmd := cacheStatsCmd
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Help(); err != nil {
		t.Errorf("help should not error: %v", err)
	}
	output := buf.String()

	if !strings.Contains(output, "cache") {
		t.Errorf("help output should contain 'cache', got: %s", output)
	}
	if !strings.Contains(output, "Anthropic") {
		t.Errorf("help output should mention Anthropic, got: %s", output)
	}
}

func TestCacheStatsCmd_NoTraffic(t *testing.T) {
	resetCacheStatsFlags(t)
	cache.GlobalCacheStatsTracker().Reset()

	stats := cache.GlobalCacheStatsTracker().GetStats()
	if stats.HasTraffic() {
		t.Fatalf("tracker should start with no traffic, got %+v", stats)
	}
}

func TestCacheStatsCmd_JsonFlag(t *testing.T) {
	resetCacheStatsFlags(t)
	cache.GlobalCacheStatsTracker().Reset()
	cache.GlobalCacheStatsTracker().RecordRequest(cache.ProviderAnthropic, true, 100)

	tracker := cache.GlobalCacheStatsTracker()
	before := tracker.GetStats()
	if before.AnthropicRequests != 1 {
		t.Fatalf("test setup: expected 1 anthropic request, got %d", before.AnthropicRequests)
	}
}

func TestCacheStatsCmd_ModelFlag(t *testing.T) {
	resetCacheStatsFlags(t)
	cache.GlobalCacheStatsTracker().Reset()
	cache.GlobalCacheStatsTracker().RecordRequest(cache.ProviderAnthropic, false, 50)

	if _, ok := cache.ModelCost("claude-3-5-sonnet-20241022"); !ok {
		t.Fatal("test prerequisite: model should exist in pricing table")
	}
}

func TestCacheStatsCmd_JsonAndModelFlags(t *testing.T) {
	resetCacheStatsFlags(t)
	cache.GlobalCacheStatsTracker().Reset()
	cache.GlobalCacheStatsTracker().RecordRequest(cache.ProviderAnthropic, true, 80)

	if _, ok := cache.ModelCost("claude-3-5-haiku-20241022"); !ok {
		t.Fatal("test prerequisite: model should exist in pricing table")
	}
}

func TestCacheStatsCmd_UnknownModelWarningEmitted(t *testing.T) {
	var captured bytes.Buffer
	handler := slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelWarn})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)

	cache.GlobalCacheStatsTracker().Reset()
	cache.GlobalCacheStatsTracker().RecordRequest(cache.ProviderAnthropic, true, 10)

	fetcher := cacheStatsCmd
	fetcher.SetArgs([]string{"--model", "gpt-4-fake"})

	oldJSON := cacheStatsFlags.jsonOut
	oldModel := cacheStatsFlags.model
	cacheStatsFlags.jsonOut = false
	cacheStatsFlags.model = "gpt-4-fake"
	defer func() {
		cacheStatsFlags.jsonOut = oldJSON
		cacheStatsFlags.model = oldModel
	}()

	runCacheStats(fetcher, nil)

	if !strings.Contains(captured.String(), "unknown model") {
		t.Errorf("expected unknown-model warning via slog, got: %q", captured.String())
	}
}

func TestCacheStatsCmd_OtherProviderOnly_NoTraffic(t *testing.T) {
	resetCacheStatsFlags(t)
	cache.GlobalCacheStatsTracker().Reset()
	cache.GlobalCacheStatsTracker().RecordRequest(cache.ProviderOther, false, 100)

	stats := cache.GlobalCacheStatsTracker().GetStats()
	if stats.HasTraffic() {
		t.Fatalf("test setup: HasTraffic should be false with only Other provider, got %+v", stats)
	}
}

func TestCacheStatsCmd_FormatJSON_IsValid(t *testing.T) {
	cache.GlobalCacheStatsTracker().Reset()
	cache.GlobalCacheStatsTracker().RecordRequest(cache.ProviderAnthropic, true, 100)
	cache.GlobalCacheStatsTracker().RecordCacheHit(50)

	stats := cache.GlobalCacheStatsTracker().GetStats()
	out := stats.FormatJSON()
	if !strings.Contains(out, "total_requests") {
		t.Errorf("expected JSON to contain total_requests, got: %s", out)
	}
	if !strings.Contains(out, "cache_hits") {
		t.Errorf("expected JSON to contain cache_hits, got: %s", out)
	}
}

func writeSemanticSnapshot(t *testing.T, home string, stats cache.SemanticCacheStats) {
	t.Helper()
	dir := filepath.Join(home, ".leanproxy", "cache")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	snap := cache.SemanticStatsSnapshot{
		Version:   1,
		UpdatedAt: time.Now(),
		Stats:     stats,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "semantic-stats.json"), data, 0600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

func TestCacheCmd_SemanticFlag_Markdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSemanticSnapshot(t, home, cache.SemanticCacheStats{
		TotalRequests: 10,
		ExactHits:     6,
		SemanticHits:  2,
		Misses:        2,
		AvgSimilarity: 0.97,
	})

	old := cacheFlags.jsonOut
	t.Cleanup(func() { cacheFlags.jsonOut = old })
	cacheFlags.jsonOut = false

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	showSemanticCacheStats(cmd)

	out := buf.String()
	for _, want := range []string{"Total Requests", "Exact Hits", "Semantic Hits", "Misses", "Hit Rate", "Avg Similarity", "80.00%"} {
		if !strings.Contains(out, want) {
			t.Errorf("semantic dashboard output missing %q:\n%s", want, out)
		}
	}
}

func TestCacheCmd_SemanticFlag_JSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSemanticSnapshot(t, home, cache.SemanticCacheStats{TotalRequests: 5, ExactHits: 5})

	old := cacheFlags.jsonOut
	t.Cleanup(func() { cacheFlags.jsonOut = old })
	cacheFlags.jsonOut = true

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	showSemanticCacheStats(cmd)

	var parsed map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("--semantic --json must emit valid JSON: %v\noutput: %s", err, buf.String())
	}
	if parsed["total_requests"] != float64(5) {
		t.Errorf("total_requests = %v, want 5", parsed["total_requests"])
	}
}

func TestCacheCmd_SemanticFlag_UnavailableJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no snapshot file written

	old := cacheFlags.jsonOut
	t.Cleanup(func() { cacheFlags.jsonOut = old })
	cacheFlags.jsonOut = true

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)

	showSemanticCacheStats(cmd)

	var parsed map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unavailable stats must still emit valid JSON: %v\noutput: %s", err, buf.String())
	}
	if parsed["status"] != "unavailable" {
		t.Errorf("status = %v, want unavailable", parsed["status"])
	}
}
