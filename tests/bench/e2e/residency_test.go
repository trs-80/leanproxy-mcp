package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/reporter"
)

func TestWriteReportRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	in := []Record{{
		Layer:           "residency",
		Arm:             "router",
		BallastServers:  2,
		BallastTools:    50,
		PayloadBytes:    1234,
		ResidencyTokens: 309,
	}}

	if err := WriteReport(path, in); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out []Record
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0].ResidencyTokens != 309 || out[0].Arm != "router" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

// ballastPoints is the sweep. The production setup contributes roughly 10 real
// tools (codebase-memory 8 after its include filter, context7 2), so the sweep
// reaches well past that to find where the proxy's floor plus round trips stop
// being worth paying.
//
// The small points are not padding. Task 3 measured the router arm's fixed
// wrapper floor at ~2174 B, which EXCEEDS a native payload below roughly 8
// tools — the router crossover sits between 4 and 8, and that crossover is the
// breakeven this harness exists to find. A sweep starting at 25 would step
// straight over it.
//
// Zero is deliberately absent: with no servers configured the proxy has nothing
// to proxy and `Capture(ArmRouter, ...)` fails with "read initialize: EOF".
var ballastPoints = []int{2, 4, 8, 25, 50, 100, 200}

func TestResidencySweep(t *testing.T) {
	if testing.Short() {
		t.Skip("residency sweep builds two binaries; skipped in -short")
	}

	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	est := reporter.NewEstimator()

	var recs []Record
	for _, tools := range ballastPoints {
		servers, perServer := 0, 0
		if tools > 0 {
			servers = 2 // split the load so the shape is not one giant server
			perServer = tools / servers
		}
		specs := BallastSpecs(mock, servers, perServer)
		// Record the tool count actually created, not the nominal sweep point:
		// integer division can drop a tool and the report must not claim
		// otherwise.
		actual := servers * perServer

		for _, arm := range AllArms() {
			payload, err := Capture(arm, lp, specs, t.TempDir())
			if err != nil {
				t.Fatalf("capture arm=%s tools=%d: %v", arm, tools, err)
			}
			recs = append(recs, Record{
				Layer:           "residency",
				Origin:          "measured",
				Arm:             string(arm),
				BallastServers:  servers,
				BallastTools:    actual,
				PayloadBytes:    len(payload),
				ResidencyTokens: est.EstimateTokens(string(payload)),
			})
			t.Logf("arm=%-7s ballast_tools=%3d bytes=%7d residency_tokens=%6d",
				arm, actual, len(payload), est.EstimateTokens(string(payload)))
		}
	}

	if err := os.MkdirAll("../../../bench-results", 0o755); err != nil {
		t.Fatalf("mkdir bench-results: %v", err)
	}
	out := filepath.Join("../../../bench-results",
		fmt.Sprintf("e2e-residency-%s.json", time.Now().Format("20060102-150405")))
	if err := WriteReport(out, recs); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	t.Logf("wrote %s", out)
}

// TestResidencyOrderingHoldsAcrossSweep asserts the cost ordering the design
// predicts, at every sweep point rather than at one.
func TestResidencyOrderingHoldsAcrossSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries; skipped in -short")
	}

	mock := buildMockMCP(t)
	lp := buildLeanproxy(t)
	est := reporter.NewEstimator()

	for _, tools := range []int{50, 200} {
		specs := BallastSpecs(mock, 2, tools/2)
		got := map[Arm]int{}
		for _, arm := range AllArms() {
			payload, err := Capture(arm, lp, specs, t.TempDir())
			if err != nil {
				t.Fatalf("capture arm=%s: %v", arm, err)
			}
			got[arm] = est.EstimateTokens(string(payload))
		}
		if !(got[ArmRouter] < got[ArmLazy] && got[ArmLazy] < got[ArmNative]) {
			t.Errorf("ballast=%d: expected router < lazy < native, got router=%d lazy=%d native=%d",
				tools, got[ArmRouter], got[ArmLazy], got[ArmNative])
		}
	}
}
