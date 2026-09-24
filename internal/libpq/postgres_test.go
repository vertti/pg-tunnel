package libpq_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
)

const postgresPassword = "test-postgres-password" //nolint:gosec // A disposable local test database credential.

func TestVerifySCRAMWithPostgresAndIgnoresAmbientSettings(t *testing.T) {
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("initdb must be on PATH in CI for the PostgreSQL authentication test")
		}
		t.Skip("install PostgreSQL and put initdb on PATH to run the SCRAM integration test")
	}
	target, port := startPostgres(t, initdb)
	for key, value := range map[string]string{
		"PGSERVICE": "unrelated", "PGSERVICEFILE": "/nonexistent/service", "PGHOST": "wrong.example",
		"PGPORT": "invalid", "PGUSER": "wrong", "PGDATABASE": "wrong", "PGPASSWORD": "wrong",
		"PGPASSFILE": "/nonexistent/passfile", "PGSSLROOTCERT": "/nonexistent/ca", "PGSSLCERT": "/nonexistent/cert",
		"PGSSLMODE": "disable", "PGCONNECT_TIMEOUT": "invalid", "PGTARGETSESSIONATTRS": "read-only",
		"PGCHANNELBINDING": "invalid", "PGREQUIREAUTH": "none", "PGMINPROTOCOLVERSION": "invalid",
		"PGMAXPROTOCOLVERSION": "invalid", "PGSSLNEGOTIATION": "invalid", "PGOPTIONS": "-c invalid_setting=value",
	} {
		t.Setenv(key, value)
	}
	require.NoError(t, libpq.Verify(t.Context(), target, port, session.Credential{Secret: postgresPassword}, nil))
	err = libpq.Verify(t.Context(), target, port, session.Credential{Secret: "rejected-password"}, nil)
	require.ErrorContains(t, err, "TLS/database connection failed")
	assert.NotContains(t, err.Error(), "rejected-password")
	var warnings []string
	require.NoError(t, libpq.Verify(t.Context(), target, port, session.Credential{Secret: postgresPassword}, func(message string) { warnings = append(warnings, message) }))
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `Database user "postgres" has privileged access`)
	assert.Contains(t, warnings[0], "SUPERUSER")
	assert.Contains(t, warnings[0], "CREATEROLE")
	assert.NotContains(t, warnings[0], postgresPassword)
	// IAM must still require its own cleartext-over-TLS exchange, not SCRAM.
	err = libpq.Verify(t.Context(), target, port, session.Credential{Secret: postgresPassword, ExpiresAt: time.Now().Add(time.Minute)}, nil)
	require.ErrorContains(t, err, "require_auth")
}

func startPostgres(t *testing.T, initdb string) (target session.Target, port int) {
	t.Helper()
	dir := t.TempDir()
	database := filepath.Join(dir, "data")
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	certServer.Close()
	cert := certServer.Certificate()
	ca := filepath.Join(dir, "server.crt")
	key := filepath.Join(dir, "server.key")
	privateKey, err := x509.MarshalPKCS8PrivateKey(certServer.TLS.Certificates[0].PrivateKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	require.NoError(t, os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600))
	passwordFile := filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(passwordFile, []byte(postgresPassword+"\n"), 0o600))
	runPostgresTool(t, initdb, "-D", database, "--no-locale", "--encoding=UTF8", "--username=postgres", "--auth-local=trust", "--auth-host=scram-sha-256", "--pwfile="+passwordFile)
	var listen net.ListenConfig
	listener, err := listen.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port = address.Port
	require.NoError(t, listener.Close())
	pgctl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	quote := func(value string) string {
		return "'" + strings.NewReplacer("\\", "\\\\", "'", "''").Replace(value) + "'"
	}
	settings := "unix_socket_directories=''\nssl=on\nssl_cert_file=" + quote(ca) + "\nssl_key_file=" + quote(key) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(database, "postgresql.auto.conf"), []byte(settings), 0o600))
	options := "-F -h 127.0.0.1 -p " + strconv.Itoa(port)
	t.Cleanup(func() {
		if _, statErr := os.Stat(filepath.Join(database, "postmaster.pid")); os.IsNotExist(statErr) {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
		defer cancel()
		output, stopErr := exec.CommandContext(ctx, pgctl, "-D", database, "-m", "immediate", "-w", "stop").CombinedOutput() //nolint:gosec // The executable is the locally discovered PostgreSQL test control tool.
		assert.NoError(t, stopErr, "%s", output)
	})
	runPostgresTool(t, pgctl, "-D", database, "-l", filepath.Join(dir, "postgres.log"), "-w", "-t", "10", "-o", options, "start")
	return session.Target{Host: cert.DNSNames[0], Port: port, Database: "postgres", User: "postgres", RootCert: ca}, port
}

func runPostgresTool(t *testing.T, program string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, program, args...).CombinedOutput() //nolint:gosec // The executable is discovered locally; arguments belong to this isolated test database.
	require.NoError(t, err, "%s", output)
}

func TestPrivilegeWarningsForRoleMembershipAndRestrictedCatalogs(t *testing.T) {
	t.Parallel()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		t.Skip("PostgreSQL is not installed")
	}
	target, port := startPostgres(t, initdb)
	credential := session.Credential{Secret: postgresPassword}
	client, err := (libpq.Files{Root: filepath.Join(t.TempDir(), "sessions")}).Prepare(target, port, credential)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	executeSQL := func(query string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), filepath.Join(filepath.Dir(initdb), "psql"), "-X", "-v", "ON_ERROR_STOP=1", "-c", query) //nolint:gosec // Local test PostgreSQL tools and fixture SQL.
		cmd.Env = client.Env(os.Environ())
		output, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "%s", output)
	}
	executeSQL("CREATE ROLE reader LOGIN PASSWORD '" + postgresPassword + "'; CREATE ROLE rds_superuser; CREATE ROLE administrator LOGIN PASSWORD '" + postgresPassword + "'; GRANT rds_superuser TO administrator;")
	for _, user := range []string{"reader", "administrator"} {
		target.User = user
		var warnings []string
		require.NoError(t, libpq.Verify(t.Context(), target, port, credential, func(message string) { warnings = append(warnings, message) }))
		if user == "reader" {
			assert.Empty(t, warnings)
		} else {
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0], "rds_superuser")
			assert.NotContains(t, warnings[0], "SUPERUSER")
		}
	}
	executeSQL("REVOKE SELECT ON pg_catalog.pg_roles FROM PUBLIC")
	target.User = "reader"
	var warnings []string
	require.NoError(t, libpq.Verify(t.Context(), target, port, credential, func(message string) { warnings = append(warnings, message) }))
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "access level is unknown")
}
