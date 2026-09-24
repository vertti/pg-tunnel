package profile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/profile"
)

func TestSavePreservesProfilesAndRefusesReplacement(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "profiles.json")
	p := profile.Profile{DBInstance: "example", Database: "data", User: "reader", Target: "i-example", RootCert: "ca.pem", Port: 5432}
	require.NoError(t, profile.Save(path, "first", &p))
	p.User = "another-reader"
	require.NoError(t, profile.Save(path, "second", &p))
	first, err := profile.Load(path, "first")
	require.NoError(t, err)
	assert.Equal(t, "reader", first.User)
	second, err := profile.Load(path, "second")
	require.NoError(t, err)
	assert.Equal(t, p.User, second.User)
	assert.Equal(t, filepath.Join(filepath.Dir(path), "ca.pem"), second.RootCert)
	before, err := os.ReadFile(path) //nolint:gosec // The path is inside t.TempDir.
	require.NoError(t, err)
	require.ErrorContains(t, profile.Save(path, "first", &p), "already exists")
	after, err := os.ReadFile(path) //nolint:gosec // The path is inside t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, before, after)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary files remain")
}

func TestSaveRefusesUnsafeDestinations(t *testing.T) {
	t.Parallel()
	p := profile.Profile{DBInstance: "example", Database: "data", User: "reader", Target: "i-example", RootCert: "ca.pem", Port: 5432}
	for name, content := range map[string]string{"malformed": "{", "unknown field": `{"profiles":{},"unexpected":true}`, "extra object": `{"profiles":{}} {}`, "oversized": `{"profiles":{}}` + strings.Repeat(" ", 1<<20)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "profiles.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			require.Error(t, profile.Save(path, "dev", &p))
			actual, err := os.ReadFile(path) //nolint:gosec // The path is inside t.TempDir.
			require.NoError(t, err)
			assert.Equal(t, content, string(actual))
		})
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "profiles.json")
	require.NoError(t, os.Symlink("missing", path))
	require.ErrorContains(t, profile.Save(path, "dev", &p), "refusing to replace")
	require.NoError(t, os.Remove(path))
	lock, err := os.Open(directory) //nolint:gosec // The directory is owned by this test.
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	require.ErrorContains(t, profile.Save(path, "dev", &p), "retry after the other setup")
	assert.NoFileExists(t, path)
}

func TestSaveRejectsInvalidInputBeforeWriting(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "profiles.json")
	valid := profile.Profile{DBInstance: "example", Database: "data", User: "reader", Target: "i-example", Port: 5432}
	for _, name := range []string{"", " padded", "line\nbreak"} {
		require.ErrorContains(t, profile.Save(path, name, &valid), "profile name")
	}
	require.ErrorContains(t, profile.Save(path, "dev", &profile.Profile{}), "validate profile to save")
	blocker := filepath.Join(directory, "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	require.ErrorContains(t, profile.Save(filepath.Join(blocker, "profiles.json"), "dev", &valid), "create configuration directory")
	assert.NoFileExists(t, path)
}

func TestSaveRefusesToGrowPastSizeLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profiles.json")
	content := `{"profiles":{"large":{"database":"` + strings.Repeat("x", (1<<20)-200) + `"}}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	p := profile.Profile{DBInstance: "example", Database: "data", User: "reader", Target: "i-example", Port: 5432}
	require.ErrorContains(t, profile.Save(path, "dev", &p), "exceeds the 1 MiB size limit")
	actual, err := os.ReadFile(path) //nolint:gosec // The path is inside t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, content, string(actual))
}
