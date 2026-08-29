package statusfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateTruncation_PersistsAndRoundTrips(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := newFileStatusStoreWithDir(nil, tmpDir)
	require.NoError(t, err)

	store.UpdateTruncation(map[string]TruncationStat{
		"cbm/search_code": {TruncatedCalls: 3, BytesBefore: 90000, BytesAfter: 24300},
	})

	info, err := readCurrentStatusFromFile(store.GetFilePath())
	require.NoError(t, err)
	require.Contains(t, info.Truncation, "cbm/search_code")
	got := info.Truncation["cbm/search_code"]
	assert.Equal(t, int64(3), got.TruncatedCalls)
	assert.Equal(t, int64(90000), got.BytesBefore)
	assert.Equal(t, int64(24300), got.BytesAfter)
}

func TestUpdateTruncation_EmptyMapOmittedFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := newFileStatusStoreWithDir(nil, tmpDir)
	require.NoError(t, err)

	store.UpdateTruncation(nil)
	store.UpdateServers([]ServerStatus{{Name: "s", Status: "running"}})

	info, err := readCurrentStatusFromFile(store.GetFilePath())
	require.NoError(t, err)
	assert.Empty(t, info.Truncation)
}
