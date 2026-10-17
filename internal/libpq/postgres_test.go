package libpq_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
	"github.com/vertti/pg-tunnel/internal/testutil"
)

const postgresPassword = testutil.PostgresPassword

func TestVerifySCRAMWithPostgresAndIgnoresAmbientSettings(t *testing.T) {
	target, port := startPostgres(t)
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
	err := libpq.Verify(t.Context(), target, port, session.Credential{Secret: "rejected-password"}, nil)
	require.EqualError(t, err, `database rejected login: password authentication failed for user "postgres"; check that the secret's password matches the database; users with rds_iam must use IAM authentication`)
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

func startPostgres(t *testing.T) (target session.Target, port int) {
	t.Helper()
	postgres := testutil.StartPostgres(t)
	return postgres.Target, postgres.Port
}

func TestPrivilegeWarningsForRoleMembershipAndRestrictedCatalogs(t *testing.T) {
	t.Parallel()
	postgres := testutil.StartPostgres(t)
	target, port := postgres.Target, postgres.Port
	credential := session.Credential{Secret: postgresPassword}
	client, err := (libpq.Files{Root: filepath.Join(t.TempDir(), "sessions")}).Prepare(target, port, credential)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	executeSQL := func(query string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), filepath.Join(postgres.Bin, "psql"), "-X", "-v", "ON_ERROR_STOP=1", "-c", query)
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
