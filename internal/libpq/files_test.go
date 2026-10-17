package libpq_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgpassfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/atomicfile"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
	"github.com/vertti/pg-tunnel/internal/testutil"
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
	client, err := factory.Prepare(target(), 15432, session.Credential{Secret: `token:with\escapes`})
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
	data, err := os.ReadFile(passwordFile)
	require.NoError(t, err)
	parsed, err := pgpassfile.ParsePassfile(strings.NewReader(string(data)))
	require.NoError(t, err)
	for _, host := range []string{"db.example", "127.0.0.1"} {
		assert.Equal(t, `token:with\escapes`, parsed.FindPassword(host, "15432", "data", "reader"), host)
	}
	assert.Empty(t, parsed.FindPassword("localhost", "15432", "data", "reader"), "localhost may resolve to ::1, which the tunnel does not own")
	require.NoError(t, client.Update(session.Credential{Secret: "replacement"}))
	data, err = os.ReadFile(passwordFile)
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
	removed, err := factory.Recover()
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "live sessions are kept")
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
	require.NoError(t, os.Chmod(root, 0o755))
	_, err := (libpq.Files{Root: root}).Prepare(target(), 12345, session.Credential{Secret: "secret"})
	require.ErrorContains(t, err, "0700")
	require.NoError(t, os.Chmod(root, 0o700))
	bad := target()
	bad.Database = "data\nsslmode=disable"
	_, err = (libpq.Files{Root: root}).Prepare(bad, 12345, session.Credential{Secret: "secret"})
	require.Error(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRecoveryWaitsForStorageLock(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	holder, err := os.Open(root)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(holder.Fd()), unix.LOCK_EX))
	recovered := make(chan error, 1)
	go func() { _, recoverErr := (libpq.Files{Root: root}).Recover(); recovered <- recoverErr }()
	select {
	case <-recovered:
		t.Fatal("recovery ran while another session held the storage lock")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, holder.Close())
	select {
	case err = <-recovered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not resume after the lock was released")
	}
}

func TestStorageMustBeARealDirectory(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "real")
	require.NoError(t, os.Mkdir(target, 0o700))
	link := filepath.Join(directory, "link")
	require.NoError(t, os.Symlink(target, link))
	_, err := (libpq.Files{Root: link}).Recover()
	require.ErrorContains(t, err, "real directory")
	file := filepath.Join(directory, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = (libpq.Files{Root: file}).Recover()
	require.ErrorContains(t, err, "create session storage")
}

func TestTLSConfigRequiresReadableCertificates(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing.pem")
	_, err := libpq.TLSConfig(session.Target{RootCert: missing})
	require.ErrorIs(t, err, os.ErrNotExist)
	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("no certificates"), 0o600))
	_, err = libpq.TLSConfig(session.Target{RootCert: empty})
	require.ErrorContains(t, err, "no usable PEM")
}

func TestRecoveryAfterSIGKILL(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	child := testutil.SelfCommand(t, "TestCredentialOwnerHelper", "PG_TUNNEL_TEST_ROOT="+root)
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		if child.ProcessState == nil {
			require.NoError(t, child.Process.Kill())
			require.Error(t, child.Wait())
		}
	})
	marker := filepath.Join(root, "ready")
	require.Eventually(t, func() bool { _, statErr := os.Stat(marker); return statErr == nil }, 5*time.Second, 10*time.Millisecond)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	dir := string(data)
	removed, err := (libpq.Files{Root: root}).Recover()
	require.NoError(t, err)
	assert.Zero(t, removed)
	_, err = os.Stat(dir)
	require.NoError(t, err, "a live owner's credentials must be retained")
	require.NoError(t, child.Process.Kill())
	require.Error(t, child.Wait())
	removed, err = (libpq.Files{Root: root}).Recover()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	_, err = os.Stat(dir)
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
	require.NoError(t, atomicfile.Write(filepath.Join(root, "ready"), []byte(dir)))
	time.Sleep(time.Hour)
}

func sharedFiles(t *testing.T, existing string) (files libpq.Files, reports *[]string) {
	t.Helper()
	directory := t.TempDir()
	pgpass := filepath.Join(directory, ".pgpass")
	if existing != "" {
		require.NoError(t, os.WriteFile(pgpass, []byte(existing), 0o600))
	}
	reports = new([]string)
	return libpq.Files{Root: filepath.Join(directory, "sessions"), Pgpass: pgpass, LocalPort: 15000, Report: func(message string) { *reports = append(*reports, message) }}, reports
}

func sharedPassword(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	parsed, err := pgpassfile.ParsePassfile(strings.NewReader(string(data)))
	require.NoError(t, err)
	return parsed.FindPassword("127.0.0.1", "15000", "data", "reader")
}

func TestSharedPgpassFollowsSessionAndKeepsOtherLines(t *testing.T) {
	t.Parallel()
	own := "# my entries\n127.0.0.1:15000:data:reader:stale\nother:5432:*:me:mine\n"
	files, reports := sharedFiles(t, own)
	client, err := files.Prepare(target(), 15000, session.Credential{Secret: `token:with\escapes`})
	require.NoError(t, err)
	assert.Equal(t, `token:with\escapes`, sharedPassword(t, files.Pgpass), "the managed block wins over older lines")
	info, err := os.Stat(files.Pgpass)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Len(t, *reports, 1)
	assert.Contains(t, (*reports)[0], "host 127.0.0.1, port 15000")
	assert.Contains(t, (*reports)[0], "postgresql://reader@127.0.0.1:15000/data?sslmode=require")
	require.NoError(t, client.Update(session.Credential{Secret: "replacement"}))
	assert.Equal(t, "replacement", sharedPassword(t, files.Pgpass))
	require.NoError(t, client.Close())
	data, err := os.ReadFile(files.Pgpass)
	require.NoError(t, err)
	assert.Equal(t, own, string(data))
}

func TestSharedPgpassOnlyForTheFixedPort(t *testing.T) {
	t.Parallel()
	for name, files := range map[string]func(libpq.Files) libpq.Files{
		"fallback port": func(f libpq.Files) libpq.Files { return f },
		"no fixed port": func(f libpq.Files) libpq.Files { f.LocalPort = 0; return f },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base, reports := sharedFiles(t, "")
			client, err := files(base).Prepare(target(), 15001, session.Credential{Secret: "token"})
			require.NoError(t, err)
			require.NoError(t, client.Update(session.Credential{Secret: "replacement"}))
			require.NoError(t, client.Close())
			assert.NoFileExists(t, base.Pgpass)
			assert.Empty(t, *reports)
		})
	}
}

func TestAnyPortSharesPgpassForTheSessionPort(t *testing.T) {
	t.Parallel()
	for name, localPort := range map[string]int{"fallback port": 15000, "no fixed port": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files, reports := sharedFiles(t, "")
			files.LocalPort, files.AnyPort = localPort, true
			client, err := files.Prepare(target(), 15001, session.Credential{Secret: "token"})
			require.NoError(t, err)
			data, err := os.ReadFile(files.Pgpass)
			require.NoError(t, err)
			parsed, err := pgpassfile.ParsePassfile(strings.NewReader(string(data)))
			require.NoError(t, err)
			assert.Equal(t, "token", parsed.FindPassword("127.0.0.1", "15001", "data", "reader"))
			require.Len(t, *reports, 1)
			assert.Contains(t, (*reports)[0], "postgresql://reader@127.0.0.1:15001/data?sslmode=require")
			require.NoError(t, client.Close())
			data, err = os.ReadFile(files.Pgpass)
			require.NoError(t, err)
			assert.Empty(t, string(data))
		})
	}
}

func TestSharedPgpassWritesThroughSymlink(t *testing.T) {
	t.Parallel()
	files, _ := sharedFiles(t, "")
	target := filepath.Join(t.TempDir(), "dotfiles-pgpass")
	require.NoError(t, os.WriteFile(target, []byte("other:5432:*:me:mine\n"), 0o600))
	require.NoError(t, os.Symlink(target, files.Pgpass))
	client, err := files.Prepare(session.Target{Host: "db.example", Port: 5432, Database: "data", User: "reader", RootCert: "/tmp/ca.pem"}, 15000, session.Credential{Secret: "token"})
	require.NoError(t, err)
	info, err := os.Lstat(files.Pgpass)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode().Type())
	assert.Equal(t, "token", sharedPassword(t, target))
	require.NoError(t, client.Close())
}

func TestRecoveryRemovesSharedBlocksOfDeadSessionsOnly(t *testing.T) {
	t.Parallel()
	dead := "# pg-tunnel session-dead begin (managed; removed when the session ends)\nlocalhost:15009:data:reader:old\n# pg-tunnel session-dead end\n"
	files, _ := sharedFiles(t, dead+"other:5432:*:me:mine\n")
	live, err := files.Prepare(target(), 15000, session.Credential{Secret: "token"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, live.Close()) })
	_, err = files.Recover()
	require.NoError(t, err)
	data, err := os.ReadFile(files.Pgpass)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "session-dead")
	assert.Contains(t, string(data), "other:5432:*:me:mine\n")
	assert.Equal(t, "token", sharedPassword(t, files.Pgpass))
}

func TestRecoveryRemovesPasswordCopiesLeftByAKilledWrite(t *testing.T) {
	t.Parallel()
	files, _ := sharedFiles(t, "")
	directory := filepath.Dir(files.Pgpass)
	leftover := filepath.Join(directory, ".pgpass.tmp-123")
	unrelated := filepath.Join(directory, ".pgpass.bak")
	for _, path := range []string{leftover, unrelated} {
		require.NoError(t, os.WriteFile(path, []byte("127.0.0.1:15000:data:reader:secret\n"), 0o600))
	}
	_, err := files.Recover()
	require.NoError(t, err)
	assert.NoFileExists(t, leftover)
	assert.FileExists(t, unrelated)
}

func TestUnterminatedSharedBlockIsLeftAlone(t *testing.T) {
	t.Parallel()
	broken := "# pg-tunnel session-dead begin (managed; removed when the session ends)\nother:5432:*:me:mine\n"
	files, _ := sharedFiles(t, broken)
	_, err := files.Recover()
	require.NoError(t, err)
	data, err := os.ReadFile(files.Pgpass)
	require.NoError(t, err)
	assert.Equal(t, broken, string(data))
}
