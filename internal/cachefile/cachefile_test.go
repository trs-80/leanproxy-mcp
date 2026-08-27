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

// A failed write must leave THE TARGET's previous contents intact. The write
// has to be made to fail on the very file being asserted on: pointing it at
// some other path would leave this passing even for a naive truncating
// os.WriteFile, which is the implementation the temp file exists to avoid.
func TestWriteAtomicFailureKeepsTargetIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	require.NoError(t, WriteAtomic(path, []byte("original"), FilePerm))

	// Read and execute but not write, so creating the sibling temp file fails.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	require.Error(t, WriteAtomic(path, []byte("replacement"), FilePerm))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "original", string(data), "failed write clobbered the target")
}

// The other half: the temp file is written in full and only the final swap
// fails, which must still leave no debris behind.
func TestWriteAtomicFailedRenameCleansUp(t *testing.T) {
	dir := t.TempDir()

	// Renaming onto an existing directory fails.
	blocked := filepath.Join(dir, "blocked.json")
	require.NoError(t, os.Mkdir(blocked, 0o700))

	err := WriteAtomic(blocked, []byte("replacement"), FilePerm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rename temp file")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.Contains(e.Name(), ".tmp"), "temp file %q left behind", e.Name())
	}
}

func TestWriteAtomicMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "cache.json")
	err := WriteAtomic(path, []byte("data"), FilePerm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create temp file")
}

// Exercised through DirUnder so the test never touches the developer's real
// home. Dir itself only supplies os.UserHomeDir, covered separately below.
func TestDirUnderCreatesSubdirectory(t *testing.T) {
	home := t.TempDir()

	dir, err := DirUnder(home, "cachefile-test")
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, Root, "cachefile-test"), dir)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, DirPerm, info.Mode().Perm())
}

// Dir must resolve home from $HOME. user.Current reads the passwd database
// and ignores it, which is what made this untestable and what breaks a
// CGO_ENABLED=0 binary running as a UID with no passwd entry.
func TestDirHonorsHOME(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := Dir("cachefile-test")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, Root, "cachefile-test"), dir)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}
