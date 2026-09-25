package libpq_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgxexample "github.com/vertti/pg-tunnel/examples/pgxpool"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
)

func TestPgxPoolExampleWithPostgres(t *testing.T) {
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("initdb must be on PATH in CI for client compatibility tests")
		}
		t.Skip("install PostgreSQL and put initdb on PATH to test client compatibility")
	}
	target, port := startPostgres(t, initdb)
	files := libpq.Files{Root: filepath.Join(t.TempDir(), "sessions")}
	first, err := files.Prepare(target, port, session.Credential{Secret: postgresPassword})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	// A second session's deliberately incorrect password must not affect the first.
	second, err := files.Prepare(target, port, session.Credential{Secret: "other-session-password"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	firstConfig := pgxExampleConfig(t, first)
	secondConfig := pgxExampleConfig(t, second)
	firstPool, err := pgxpool.NewWithConfig(t.Context(), firstConfig)
	require.NoError(t, err)
	t.Cleanup(firstPool.Close)
	secondPool, err := pgxpool.NewWithConfig(t.Context(), secondConfig)
	require.NoError(t, err)
	t.Cleanup(secondPool.Close)

	conn, err := firstPool.Acquire(t.Context())
	require.NoError(t, err)
	original := conn.Conn().Config().Copy()
	originalPID := conn.Conn().PgConn().PID()
	conn.Release()
	require.ErrorContains(t, secondPool.Ping(t.Context()), "password authentication failed")

	t.Run("unadapted hostaddr is rejected by PostgreSQL", func(t *testing.T) {
		config := original.Copy()
		config.RuntimeParams["hostaddr"] = "127.0.0.1"
		_, connectErr := pgx.ConnectConfig(t.Context(), config)
		require.ErrorContains(t, connectErr, `unrecognized configuration parameter "hostaddr"`)
	})
	t.Run("TLS still rejects a different hostname", func(t *testing.T) {
		config := original.Copy()
		config.TLSConfig = config.TLSConfig.Clone()
		config.TLSConfig.ServerName = "wrong.invalid"
		_, connectErr := pgx.ConnectConfig(t.Context(), config)
		require.ErrorContains(t, connectErr, "certificate is valid for")
		require.ErrorContains(t, connectErr, "not wrong.invalid")
	})
	t.Run("new connection reloads the atomically replaced password", func(t *testing.T) {
		const replacement = `rotated:password\with-escaping`
		_, queryErr := firstPool.Exec(t.Context(), `ALTER ROLE postgres PASSWORD 'rotated:password\with-escaping'`)
		require.NoError(t, queryErr)
		require.NoError(t, first.Update(session.Credential{Secret: replacement}))
		firstPool.Reset()
		fresh, acquireErr := firstPool.Acquire(t.Context())
		require.NoError(t, acquireErr)
		defer fresh.Release()
		assert.NotEqual(t, originalPID, fresh.Conn().PgConn().PID())
		var user string
		var ssl bool
		require.NoError(t, fresh.QueryRow(t.Context(), "SELECT current_user, ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&user, &ssl))
		assert.Equal(t, target.User, user)
		assert.True(t, ssl)
		_, connectErr := pgx.ConnectConfig(t.Context(), original)
		require.ErrorContains(t, connectErr, "password authentication failed")
		require.ErrorContains(t, secondPool.Ping(t.Context()), "password authentication failed")
		require.NoError(t, second.Update(session.Credential{Secret: replacement}))
		require.NoError(t, secondPool.Ping(t.Context()))
	})
	t.Run("missing credential fails without retaining the old password", func(t *testing.T) {
		require.NoError(t, first.Close())
		firstPool.Reset()
		require.ErrorContains(t, firstPool.Ping(t.Context()), "read pg-tunnel password file")
		secondPool.Reset()
		require.NoError(t, secondPool.Ping(t.Context()))
	})
	t.Run("no matching credential fails explicitly", func(t *testing.T) {
		config := secondConfig.ConnConfig.Copy()
		config.User = "missing-user"
		require.ErrorContains(t, secondConfig.BeforeConnect(t.Context(), config), "no matching credential")
	})
}

func pgxExampleConfig(t *testing.T, client session.Client) *pgxpool.Config {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
		}
	}
	for _, entry := range client.Env(nil) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	config, err := pgxexample.Config()
	require.NoError(t, err)
	config.MaxConns = 1
	return config
}
