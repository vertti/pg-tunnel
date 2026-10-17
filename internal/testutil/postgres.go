package testutil

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/session"
)

// PostgresPassword is the initial password of the postgres superuser.
const PostgresPassword = "test-postgres-password"

// Postgres is a TLS-only PostgreSQL server with SCRAM authentication on 127.0.0.1.
type Postgres struct {
	Target session.Target
	Port   int
	Bin    string
}

// StartPostgres skips the test when PostgreSQL is not installed, except in CI.
func StartPostgres(t *testing.T) Postgres {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("initdb must be on PATH in CI for the PostgreSQL integration tests")
		}
		t.Skip("install PostgreSQL and put initdb on PATH to run the PostgreSQL integration tests")
	}
	bin := filepath.Dir(initdb)
	dir := t.TempDir()
	database := filepath.Join(dir, "data")
	ca, certServer := TLSServerCA(t)
	key := filepath.Join(dir, "server.key")
	privateKey, err := x509.MarshalPKCS8PrivateKey(certServer.TLS.Certificates[0].PrivateKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600))
	passwordFile := filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(passwordFile, []byte(PostgresPassword+"\n"), 0o600))
	runPostgresTool(t, initdb, "-D", database, "--no-locale", "--encoding=UTF8", "--username=postgres", "--auth-local=trust", "--auth-host=scram-sha-256", "--pwfile="+passwordFile)
	var listen net.ListenConfig
	listener, err := listen.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := address.Port
	require.NoError(t, listener.Close())
	pgctl := filepath.Join(bin, "pg_ctl")
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
		output, stopErr := exec.CommandContext(ctx, pgctl, "-D", database, "-m", "immediate", "-w", "stop").CombinedOutput()
		assert.NoError(t, stopErr, "%s", output)
	})
	runPostgresTool(t, pgctl, "-D", database, "-l", filepath.Join(dir, "postgres.log"), "-w", "-t", "10", "-o", options, "start")
	target := session.Target{Host: certServer.Certificate().DNSNames[0], Port: port, Database: "postgres", User: "postgres", RootCert: ca}
	return Postgres{Target: target, Port: port, Bin: bin}
}

func runPostgresTool(t *testing.T, program string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
	require.NoError(t, err, "%s", output)
}
