package e2e

import (
	"encoding/json"
	"fmt"
	"os"
)

// Record is one measurement. Layer 1 (residency, this package) and Layer 2
// (live, Task 5+'s Python harness) both emit this shape so the two halves
// can be joined without a translation step.
//
// Layer/Origin/Arm/BallastServers/BallastTools are set by both layers.
// PayloadBytes/ResidencyTokens are set only by layer 1 (residency) — every
// residency record measures them, so they always serialize, zero included.
// Task/Turns/InputTokens/OutputTokens/CacheRead/CacheWrite/CostUSD/
// ContextTokens/Succeeded are set only by layer 2 (live) — a residency
// record never measures any of them and leaves them entirely absent from
// its JSON, not zeroed, since a residency record claiming e.g. "succeeded:
// false" or "cost_usd: 0" would assert something layer 1 never measured.
type Record struct {
	// Layer is "residency" or "live".
	Layer string `json:"layer"`
	// Origin is "measured" or "derived". The harness never interpolates
	// silently; a derived point says so.
	Origin string `json:"origin"`
	Arm    string `json:"arm"`

	BallastServers int `json:"ballast_servers"`
	BallastTools   int `json:"ballast_tools"`

	// Residency fields (layer 1). No omitempty: a genuine zero is still a
	// real measurement and must not be indistinguishable from "not set".
	PayloadBytes int `json:"payload_bytes"`
	// ResidencyTokens is an ESTIMATE (reporter.NewEstimator, ceil(bytes/4)),
	// not a tokenizer count — punctuation-dense JSON tokenises worse than 4
	// chars/token, so treat this as a comparable proxy across arms, not an
	// absolute token count.
	ResidencyTokens int `json:"residency_tokens"`

	// Live fields (layer 2). Plain zero-value fields use omitempty since an
	// absent field and a zero one mean the same thing for them (no partial
	// run reports "0 turns" as meaningfully different from "not run").
	Task         string `json:"task,omitempty"`
	Turns        int    `json:"turns,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	CacheRead    int    `json:"cache_read,omitempty"`
	CacheWrite   int    `json:"cache_write,omitempty"`
	// CostUSD and Succeeded are pointers, not plain values: a live run can
	// genuinely cost $0 or genuinely fail, and those explicit values must
	// stay distinguishable both from each other and from "not measured by
	// this layer". A plain float64/bool with omitempty collapses "$0" and
	// "unset" into the same absent field; without omitempty it forces every
	// residency record (which never runs a live task) to assert
	// "succeeded: false" — a false failure claim on every successful
	// residency measurement. *T + omitempty gives every combination its own
	// distinct JSON shape: nil -> absent (residency, not applicable),
	// non-nil -> the real value, zero included (live, measured).
	CostUSD       *float64 `json:"cost_usd,omitempty"`
	ContextTokens int      `json:"context_tokens,omitempty"`
	Succeeded     *bool    `json:"succeeded,omitempty"`
}

// WriteReport serializes recs to path as indented JSON.
func WriteReport(path string, recs []Record) error {
	body, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
