package libpq_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgpassfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
)

func target() session.Target {
	return session.Target{Host: "db.example", Port: 5432, Database: "data", User: "reader", RootCert: "/tmp/ca.pem"}
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

func TestPrivateFilesRefreshAndCleanup(t *testing.T) {
	t.Parallel()
	factory := libpq.Files{Root: filepath.Join(t.TempDir(), "sessions")}
	client, err := factory.Prepare(target(), 15432, session.Credential{Secret: `token:with\escapes`}) //nolint:gosec // Synthetic credential exercises PostgreSQL password-file escaping.
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	env := client.Env([]string{"PGPASSWORD=stale", "PGHOST=wrong", "PGSERVICE=wrong", "PATH=/bin"})
	assert.NotContains(t, env, "PGPASSWORD=stale")
	assert.NotContains(t, env, "PGHOST=wrong")
	assert.Contains(t, env, "PATH=/bin")
	passwordFile := envValue(env, "PGPASSFILE")
	for _, path := range []string{passwordFile, envValue(env, "PGSERVICEFILE")} {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	info, err := os.Stat(filepath.Dir(passwordFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	data, err := os.ReadFile(passwordFile) //nolint:gosec // Read back the credential file just created inside t.TempDir.
	require.NoError(t, err)
	parsed, err := pgpassfile.ParsePassfile(strings.NewReader(string(data)))
	require.NoError(t, err)
	assert.Equal(t, `token:with\escapes`, parsed.FindPassword("db.example", "15432", "data", "reader"))
	require.NoError(t, client.Update(session.Credential{Secret: "replacement"}))
	data, err = os.ReadFile(passwordFile) //nolint:gosec // Read back the credential file just created inside t.TempDir.
	require.NoError(t, err)
	assert.Contains(t, string(data), "replacement")
	assert.NotContains(t, string(data), "token")
	service, err := os.ReadFile(envValue(env, "PGSERVICEFILE"))
	require.NoError(t, err)
	assert.Contains(t, string(service), "host=db.example\nhostaddr=127.0.0.1\nport=15432\n")
	assert.Contains(t, string(service), "sslmode=verify-full")
	require.NoError(t, client.Close())
	_, err = os.Stat(filepath.Dir(passwordFile))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestConcurrentSessionsAndAbandonedRecovery(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	factory := libpq.Files{Root: root}
	first, err := factory.Prepare(target(), 12345, session.Credential{Secret: "first"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := factory.Prepare(target(), 12346, session.Credential{Secret: "second"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	stale := filepath.Join(root, "session-abandoned")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "pgpass"), []byte("stale secret"), 0o600))
	require.NoError(t, factory.Recover())
	_, err = os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, client := range []session.Client{first, second} {
		_, err = os.Stat(envValue(client.Env(nil), "PGPASSFILE"))
		require.NoError(t, err)
	}
	require.NoError(t, first.Close())
	_, err = os.Stat(envValue(second.Env(nil), "PGPASSFILE"))
	require.NoError(t, err)
}

func TestFailedRefreshPreservesOldPassword(t *testing.T) {
	t.Parallel()
	client, err := (libpq.Files{Root: filepath.Join(t.TempDir(), "sessions")}).Prepare(target(), 12345, session.Credential{Secret: "still-valid"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Error(t, client.Update(session.Credential{Secret: "bad\npassword"}))
	data, err := os.ReadFile(envValue(client.Env(nil), "PGPASSFILE"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "still-valid")
}

func TestUnsafeStorageAndSettingsRejected(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.Chmod(root, 0o755)) //nolint:gosec // Deliberately unsafe permissions exercise rejection.
	_, err := (libpq.Files{Root: root}).Prepare(target(), 12345, session.Credential{Secret: "secret"})
	require.ErrorContains(t, err, "0700")
	require.NoError(t, os.Chmod(root, 0o700)) //nolint:gosec // Directories require the execute bit for owner access.
	bad := target()
	bad.Database = "data\nsslmode=disable"
	_, err = (libpq.Files{Root: root}).Prepare(bad, 12345, session.Credential{Secret: "secret"})
	require.Error(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestCredentialFormattingIsRedacted(t *testing.T) {
	t.Parallel()
	credential := session.Credential{Secret: "never-log-this"}
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		assert.NotContains(t, fmt.Sprintf(format, credential), credential.Secret)
	}
}

func TestRecoveryAfterSIGKILL(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	executable, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestCredentialOwnerHelper$") //nolint:gosec // Execute this test binary in a subprocess to exercise actual process death.
	child.Env = append(os.Environ(), "PG_TUNNEL_TEST_ROOT="+root)
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		if child.ProcessState == nil {
			require.NoError(t, child.Process.Kill())
			require.Error(t, child.Wait())
		}
	})
	marker := filepath.Join(root, "ready")
	require.Eventually(t, func() bool { _, statErr := os.Stat(marker); return statErr == nil }, 5*time.Second, 10*time.Millisecond)
	data, err := os.ReadFile(marker) //nolint:gosec // The marker is written by the test child in t.TempDir.
	require.NoError(t, err)
	dir := string(data)
	require.NoError(t, (libpq.Files{Root: root}).Recover())
	_, err = os.Stat(dir) //nolint:gosec // This path comes from the test child running under t.TempDir.
	require.NoError(t, err, "a live owner's credentials must be retained")
	require.NoError(t, child.Process.Kill())
	require.Error(t, child.Wait())
	require.NoError(t, (libpq.Files{Root: root}).Recover())
	_, err = os.Stat(dir) //nolint:gosec // This path comes from the test child running under t.TempDir.
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCredentialOwnerHelper(t *testing.T) {
	t.Parallel()
	root := os.Getenv("PG_TUNNEL_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess fixture")
	}
	client, err := (libpq.Files{Root: root}).Prepare(target(), 12345, session.Credential{Secret: "crash-test"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	dir := filepath.Dir(envValue(client.Env(nil), "PGPASSFILE"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ready"), []byte(dir), 0o600)) //nolint:gosec // The parent supplies its t.TempDir path to this subprocess fixture.
	time.Sleep(time.Hour)
}
