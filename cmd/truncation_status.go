package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/mcp"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/statusfile"
)

// pushTruncationStatus copies the handler's truncation counters into the
// status file. Nothing is written while no result has been cut, so the
// status file is not churned every tick on idle or cap-free setups.
func pushTruncationStatus(store *statusfile.FileStatusStore, stats map[string]mcp.TruncationStat) {
	if store == nil || len(stats) == 0 {
		return
	}
	out := make(map[string]statusfile.TruncationStat, len(stats))
	for tool, s := range stats {
		out[tool] = statusfile.TruncationStat{
			TruncatedCalls: s.TruncatedCalls,
			BytesBefore:    s.BytesBefore,
			BytesAfter:     s.BytesAfter,
		}
	}
	store.UpdateTruncation(out)
}

// renderTruncationSummary formats the per-tool truncation counters for the
// status command. Empty input renders nothing.
func renderTruncationSummary(stats map[string]statusfile.TruncationStat) string {
	if len(stats) == 0 {
		return ""
	}
	tools := make([]string, 0, len(stats))
	for tool := range stats {
		tools = append(tools, tool)
	}
	sort.Strings(tools)

	var sb strings.Builder
	sb.WriteString("Truncation (result caps):\n")
	for _, tool := range tools {
		s := stats[tool]
		sb.WriteString(fmt.Sprintf("  %-40s %5d cut, %s saved\n",
			tool, s.TruncatedCalls, formatBytes(s.BytesBefore-s.BytesAfter)))
	}
	return sb.String()
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
