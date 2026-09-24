package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/atomicfile"
)

func TestWriteReplacesPrivatelyWithoutTemporaryFiles(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "settings")
	require.NoError(t, atomicfile.Write(path, []byte("first")))
	require.NoError(t, atomicfile.Write(path, []byte("second")))

	data, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temporary directory.
	require.NoError(t, err)
	assert.Equal(t, "second", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestWriteFailureLeavesNoTemporaryFile(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	require.Error(t, atomicfile.Write(directory, []byte("data")))
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
