package cmd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/mcp"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/statusfile"
)

func readStatusInfoForTest(t *testing.T, store *statusfile.FileStatusStore) (statusfile.StatusInfo, error) {
	t.Helper()
	var info statusfile.StatusInfo
	data, err := os.ReadFile(store.GetFilePath())
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(data, &info)
}

func TestPushTruncationStatus_WritesStatsToStatusFile(t *testing.T) {
	store, err := statusfile.NewFileStatusStoreFromConfigDir("stdio", nil, t.TempDir())
	require.NoError(t, err)

	pushTruncationStatus(store, map[string]mcp.TruncationStat{
		"cbm/search_code": {TruncatedCalls: 4, BytesBefore: 120000, BytesAfter: 33000},
	})

	info, err := readStatusInfoForTest(t, store)
	require.NoError(t, err)
	require.Contains(t, info.Truncation, "cbm/search_code")
	assert.Equal(t, int64(4), info.Truncation["cbm/search_code"].TruncatedCalls)
}

func TestPushTruncationStatus_NoStatsWritesNothing(t *testing.T) {
	store, err := statusfile.NewFileStatusStoreFromConfigDir("stdio", nil, t.TempDir())
	require.NoError(t, err)

	pushTruncationStatus(store, nil)

	// No stats and no prior write: the status file must not even exist yet
	// (pushing an empty section would churn the file every tick for nothing).
	_, err = readStatusInfoForTest(t, store)
	assert.Error(t, err)
}

func TestRenderTruncationSummary(t *testing.T) {
	out := renderTruncationSummary(map[string]statusfile.TruncationStat{
		"cbm/search_code":      {TruncatedCalls: 4, BytesBefore: 120000, BytesAfter: 33000},
		"cbm/get_architecture": {TruncatedCalls: 1, BytesBefore: 9000, BytesAfter: 6100},
	})

	assert.Contains(t, out, "Truncation")
	assert.Contains(t, out, "cbm/search_code")
	assert.Contains(t, out, "4")
	// 120000-33000 = 87000 bytes saved, rendered in KB
	assert.Contains(t, out, "85.0 KB")
	assert.Contains(t, out, "cbm/get_architecture")
}

func TestRenderTruncationSummary_EmptyIsBlank(t *testing.T) {
	assert.Empty(t, renderTruncationSummary(nil))
}
