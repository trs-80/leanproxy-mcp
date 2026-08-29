package injection

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/bouncer"
)

// DefaultQuarantineTTL is the default retention period for quarantined payloads.
const DefaultQuarantineTTL = 7 * 24 * time.Hour

type Action string

const (
	ActionBlock      Action = "block"
	ActionQuarantine Action = "quarantine"
	ActionRedact     Action = "redact"
	ActionLog        Action = "log"
)

type Rule struct {
	MinRisk int    `yaml:"min_risk"`
	MaxRisk int    `yaml:"max_risk"`
	Action  Action `yaml:"action"`
}

type ActionResult struct {
	Action             Action `json:"action"`
	Message            string `json:"message,omitempty"`
	RiskScore          int    `json:"risk_score"`
	QuarantineID       string `json:"quarantine_id,omitempty"`
	QuarantineDir      string `json:"-"`
	TransformedPayload string `json:"-"`
}

type Dispatcher struct {
	rules         []Rule
	quarantineDir string
	quarantineTTL time.Duration
	sweepOnce     sync.Once
}

func DefaultRules() []Rule {
	return []Rule{
		{MinRisk: 80, MaxRisk: 100, Action: ActionBlock},
		{MinRisk: 50, MaxRisk: 79, Action: ActionQuarantine},
		{MinRisk: 1, MaxRisk: 49, Action: ActionLog},
	}
}

func NewDispatcher(rules []Rule) *Dispatcher {
	if rules == nil {
		rules = DefaultRules()
	}
	qDir := defaultQuarantineDir()
	return &Dispatcher{
		rules:         rules,
		quarantineDir: qDir,
		quarantineTTL: DefaultQuarantineTTL,
	}
}

func NewDispatcherWithQuarantineDir(rules []Rule, quarantineDir string) *Dispatcher {
	if rules == nil {
		rules = DefaultRules()
	}
	return &Dispatcher{
		rules:         rules,
		quarantineDir: quarantineDir,
		quarantineTTL: DefaultQuarantineTTL,
	}
}

// defaultQuarantineDir returns the quarantine directory under the user's home.
// If the home directory cannot be determined it returns "" so that quarantine
// fails closed instead of persisting payloads to a shared temp directory.
func defaultQuarantineDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		slog.Warn("injection: cannot determine home dir; quarantine disabled, payloads will be blocked", "error", err)
		return ""
	}
	return filepath.Join(home, ".leanproxy", "quarantine")
}

// SetQuarantineTTL sets the retention period for quarantined files. A value
// <= 0 disables the retention sweep.
func (d *Dispatcher) SetQuarantineTTL(ttl time.Duration) {
	d.quarantineTTL = ttl
}

func (d *Dispatcher) QuarantineTTL() time.Duration {
	return d.quarantineTTL
}

// sweepQuarantine deletes quarantine files older than the configured TTL.
// It runs once per dispatcher, on the first quarantine.
func (d *Dispatcher) sweepQuarantine() {
	if d.quarantineTTL <= 0 {
		return
	}
	entries, err := os.ReadDir(d.quarantineDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-d.quarantineTTL)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(d.quarantineDir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		slog.Info("injection: removed expired quarantine files", "count", removed, "ttl", d.quarantineTTL)
	}
}

func (d *Dispatcher) Rules() []Rule {
	result := make([]Rule, len(d.rules))
	copy(result, d.rules)
	return result
}

func (d *Dispatcher) Dispatch(result Result) ActionResult {
	risk := result.RiskScore
	for _, rule := range d.rules {
		if risk >= rule.MinRisk && (rule.MaxRisk == 0 || risk <= rule.MaxRisk) {
			return d.applyAction(rule.Action, result)
		}
	}
	return ActionResult{
		Action:    ActionLog,
		Message:   "no matching rule, logging",
		RiskScore: risk,
	}
}

func (d *Dispatcher) applyAction(action Action, result Result) ActionResult {
	switch action {
	case ActionBlock:
		return d.block(result)
	case ActionQuarantine:
		return d.quarantine(result)
	case ActionRedact:
		return d.redact(result)
	case ActionLog:
		return d.logOnly(result)
	default:
		return d.logOnly(result)
	}
}

func (d *Dispatcher) block(result Result) ActionResult {
	slog.Error("injection: blocked high-risk payload",
		"risk_score", result.RiskScore,
		"matches", len(result.Matches),
	)
	return ActionResult{
		Action:    ActionBlock,
		Message:   fmt.Sprintf("BLOCKED: payload risk score %d exceeds threshold", result.RiskScore),
		RiskScore: result.RiskScore,
	}
}

func (d *Dispatcher) quarantine(result Result) ActionResult {
	id := uuid.New().String()
	qDir := d.quarantineDir
	if qDir == "" {
		slog.Error("injection: quarantine dir unknown, blocking payload instead")
		return d.block(result)
	}
	if err := os.MkdirAll(qDir, 0700); err != nil {
		slog.Error("injection: cannot create quarantine dir", "path", qDir, "error", err)
		return d.block(result)
	}
	d.sweepOnce.Do(d.sweepQuarantine)

	qPath := filepath.Join(qDir, id+".json")
	qEntry := struct {
		ID        string  `json:"id"`
		RiskScore int     `json:"risk_score"`
		Payload   string  `json:"payload"`
		Matches   []Match `json:"matches"`
	}{
		ID:        id,
		RiskScore: result.RiskScore,
		Payload:   bouncer.RedactSecrets(result.Payload),
		Matches:   result.Matches,
	}

	data, err := json.MarshalIndent(qEntry, "", "  ")
	if err != nil {
		slog.Error("injection: failed to marshal quarantine entry", "error", err)
		return d.block(result)
	}

	if err := os.WriteFile(qPath, data, 0600); err != nil {
		slog.Error("injection: failed to write quarantine file", "path", qPath, "error", err)
		return d.block(result)
	}

	slog.Warn("injection: payload quarantined",
		"id", id,
		"risk_score", result.RiskScore,
		"path", qPath,
	)

	return ActionResult{
		Action:        ActionQuarantine,
		Message:       fmt.Sprintf("[CONTENT_QUARANTINED - id %s]", id),
		RiskScore:     result.RiskScore,
		QuarantineID:  id,
		QuarantineDir: qDir,
	}
}

func (d *Dispatcher) redact(result Result) ActionResult {
	// TransformedPayload replaces req.Params, so it must be a valid JSON value.
	redacted := `"[CONTENT_REDACTED]"`
	slog.Warn("injection: redacting payload",
		"risk_score", result.RiskScore,
		"matches", len(result.Matches),
	)
	return ActionResult{
		Action:             ActionRedact,
		Message:            "[CONTENT_REDACTED]",
		RiskScore:          result.RiskScore,
		TransformedPayload: redacted,
	}
}

func (d *Dispatcher) logOnly(result Result) ActionResult {
	slog.Debug("injection: low-risk payload logged",
		"risk_score", result.RiskScore,
		"matches", len(result.Matches),
	)
	return ActionResult{
		Action:    ActionLog,
		Message:   "forwarded",
		RiskScore: result.RiskScore,
	}
}

func (d *Dispatcher) QuarantineDir() string {
	return d.quarantineDir
}

type ActionCounts struct {
	Block      int `json:"block"`
	Quarantine int `json:"quarantine"`
	Redact     int `json:"redact"`
	Log        int `json:"log"`
	Total      int `json:"total"`
}
