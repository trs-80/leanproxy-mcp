package e2e

import (
	"encoding/json"
	"fmt"
	"os"
)

// Record is one measurement. Layer 1 (residency) and Layer 2 (live) both emit
// this shape so the two halves can be joined without a translation step.
// Fields not applicable to a layer stay zero.
type Record struct {
	// Layer is "residency" or "live".
	Layer string `json:"layer"`
	// Origin is "measured" or "derived". The harness never interpolates
	// silently; a derived point says so.
	Origin string `json:"origin"`
	Arm    string `json:"arm"`

	BallastServers int `json:"ballast_servers"`
	BallastTools   int `json:"ballast_tools"`

	// Residency fields (layer 1).
	PayloadBytes    int `json:"payload_bytes,omitempty"`
	ResidencyTokens int `json:"residency_tokens,omitempty"`

	// Live fields (layer 2).
	Task          string  `json:"task,omitempty"`
	Turns         int     `json:"turns,omitempty"`
	InputTokens   int     `json:"input_tokens,omitempty"`
	OutputTokens  int     `json:"output_tokens,omitempty"`
	CacheRead     int     `json:"cache_read,omitempty"`
	CacheWrite    int     `json:"cache_write,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
	ContextTokens int     `json:"context_tokens,omitempty"`
	Succeeded     bool    `json:"succeeded,omitempty"`
}

// WriteReport serialises recs to path as indented JSON.
func WriteReport(path string, recs []Record) error {
	body, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
