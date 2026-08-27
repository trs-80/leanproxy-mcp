package cachefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with-dash", "with-dash"},
		{"with_underscore", "with_underscore"},
		{"UPPERCASE", "UPPERCASE"},
		{"with spaces", "with_spaces"},
		{"with.dot", "with_dot"},
		{"with@ special!", "with__special_"},
		{"../escape", "___escape"},
		{"nested/path", "nested_path"},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, SanitizeName(tc.input))
		})
	}
}

func TestSanitizeNameContainsNoSeparator(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", `..\..\windows`, "a/b/c"} {
		got := SanitizeName(name)
		assert.NotContains(t, got, string(os.PathSeparator))
		assert.NotContains(t, got, "/")
		assert.NotContains(t, got, `\`)
		assert.NotContains(t, got, "..")
	}
}

func TestWriteAtomicCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	require.NoError(t, WriteAtomic(path, []byte(`{"a":1}`), FilePerm))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, FilePerm, info.Mode().Perm())
}

func TestWriteAtomicReplacesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	require.NoError(t, WriteAtomic(path, []byte("old"), FilePerm))
	require.NoError(t, WriteAtomic(path, []byte("new-and-longer"), FilePerm))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new-and-longer", string(data))
}

// A successful write must not leave its temp file behind, or the cache
// directory would grow without bound as entries are refreshed.
func TestWriteAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	require.NoError(t, WriteAtomic(path, []byte("payload"), FilePerm))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "cache.json", entries[0].Name())
}

// A failed write must leave the previous contents intact and clean up after
// itself: that is the whole point of going through a temp file.
func TestWriteAtomicFailureKeepsPreviousContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	require.NoError(t, WriteAtomic(path, []byte("original"), FilePerm))

	// Renaming onto a directory fails, exercising the cleanup path after the
	// temp file has been fully written.
	blocked := filepath.Join(dir, "blocked.json")
	require.NoError(t, os.Mkdir(blocked, 0o700))

	err := WriteAtomic(blocked, []byte("replacement"), FilePerm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rename temp file")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "original", string(data))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for _, name := range names {
		assert.False(t, strings.Contains(name, ".tmp"), "temp file %q left behind", name)
	}
}

func TestWriteAtomicMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "cache.json")
	err := WriteAtomic(path, []byte("data"), FilePerm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create temp file")
}

func TestDirCreatesSubdirectory(t *testing.T) {
	dir, err := Dir("cachefile-test")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	assert.True(t, strings.HasSuffix(dir, filepath.Join(Root, "cachefile-test")))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, DirPerm, info.Mode().Perm())
}
