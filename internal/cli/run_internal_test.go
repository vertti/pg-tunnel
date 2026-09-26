package cli

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/profile"
)

func TestConnectReportsOnlyClientSettingsUntilStopped(t *testing.T) {
	t.Parallel()
	var reported []string
	var stdout bytes.Buffer
	command := sessionCommand(nil, &stdout, func(message string) { reported = append(reported, message) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, command(ctx, []string{"PGSERVICE=pg-tunnel", "AWS_SECRET_ACCESS_KEY=private-marker", "PGPASSFILE=/private/it's here/pgpass"}), "Ctrl-C is the normal way to stop connect")
	assert.Equal(t, "export PGSERVICE='pg-tunnel'\nexport PGPASSFILE='/private/it'\"'\"'s here/pgpass'\n", stdout.String())
	require.Len(t, reported, 1)
	assert.NotContains(t, reported[0], "private-marker")
}

func TestRunPassesSessionEnvironmentToCommand(t *testing.T) {
	t.Parallel()
	command := sessionCommand([]string{"sh", "-c", `test "$PGSERVICE" = pg-tunnel`}, io.Discard, func(string) {})
	require.NoError(t, command(t.Context(), []string{"PGSERVICE=pg-tunnel"}))
}

func TestEnvironmentExpiry(t *testing.T) {
	for _, tc := range []struct {
		want             time.Time
		name, key, value string
		wantErr          bool
	}{
		{name: "unset"},
		{name: "without environment credentials", value: "2030-01-02T03:04:05Z"},
		{name: "environment credentials", key: "test", value: "2030-01-02T03:04:05Z", want: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "invalid", key: "test", value: "tomorrow", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", tc.key)
			t.Setenv("AWS_CREDENTIAL_EXPIRATION", tc.value)
			expiry, err := environmentExpiry()
			if tc.wantErr {
				require.ErrorContains(t, err, "AWS_CREDENTIAL_EXPIRATION")
				return
			}
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(expiry))
		})
	}
}

func TestDatabaseAuthSelectsConfiguredMode(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	cfg := aws.Config{Region: "eu-central-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")}

	auth, err := databaseAuth(&profile.Profile{Auth: profile.AuthSecretsManager, SecretID: "chosen"}, &cfg, nil)
	require.NoError(t, err)
	secrets, ok := auth.(awsdb.Secrets)
	require.True(t, ok)
	assert.Equal(t, "chosen", secrets.ID)

	auth, err = databaseAuth(&profile.Profile{}, &cfg, nil)
	require.NoError(t, err)
	iam, ok := auth.(awsdb.IAM)
	require.True(t, ok)
	assert.Equal(t, "eu-central-1", iam.Region)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_CREDENTIAL_EXPIRATION", "invalid")
	_, err = databaseAuth(&profile.Profile{}, &cfg, nil)
	require.ErrorContains(t, err, "AWS_CREDENTIAL_EXPIRATION")
}

func TestAWSConfigRequiresRegion(t *testing.T) {
	isolateAWS(t)
	_, err := awsConfig(t.Context(), &profile.Profile{})
	require.ErrorContains(t, err, "AWS region is missing")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	cfg, err := awsConfig(t.Context(), &profile.Profile{Region: "eu-north-1"})
	require.NoError(t, err)
	assert.Equal(t, "eu-north-1", cfg.Region)
}

func TestMissingAWSLoginNamesTheFix(t *testing.T) {
	isolateAWS(t)
	ssoProfile := "[profile dev]\nsso_start_url = https://example.awsapps.com/start\nsso_region = eu-west-1\nsso_account_id = 123456789012\nsso_role_name = Reader\nregion = eu-west-1\n[profile keys]\nregion = eu-west-1\n"
	require.NoError(t, os.WriteFile(os.Getenv("AWS_CONFIG_FILE"), []byte(ssoProfile), 0o600)) //nolint:gosec // isolateAWS points this at a temporary file.
	_, err := awsConfig(t.Context(), &profile.Profile{AWSProfile: "dev"})
	require.ErrorContains(t, err, "aws sso login --profile dev")
	for _, p := range []profile.Profile{{Region: "eu-west-1"}, {AWSProfile: "keys"}} {
		_, err = awsConfig(t.Context(), &p)
		require.ErrorContains(t, err, "no usable AWS credentials; make AWS credentials available")
		require.ErrorContains(t, err, "AWS_PROFILE or the connection's aws_profile")
		require.NotContains(t, err.Error(), "aws sso login", "only SSO profiles are told to use aws sso login")
	}
}

func TestSessionStopsAtFirstFailedStage(t *testing.T) {
	isolateAWS(t)
	// Static credentials pass the login check; AWS calls fail fast against a closed local port.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	directory := t.TempDir()
	ca := testCA(t)
	invalidCA := filepath.Join(directory, "invalid.pem")
	require.NoError(t, os.WriteFile(invalidCA, []byte("not a certificate"), 0o600))
	cache, err := os.UserCacheDir()
	require.NoError(t, err)
	managed := filepath.Join(cache, "pg-tunnel", "certificates", "aws.pem")
	require.NoError(t, os.MkdirAll(filepath.Dir(managed), 0o700))
	data, err := os.ReadFile(ca) //nolint:gosec // The path is inside the test's temporary directory.
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(managed, data, 0o600)) //nolint:gosec // The path is inside the test's temporary directory.

	base := profile.Profile{Database: "data", User: "reader", Target: "i-example", Region: "eu-central-1", Port: 5432}
	for _, tc := range []struct {
		edit       func(*profile.Profile)
		name, want string
	}{
		{func(p *profile.Profile) { p.Host, p.RootCert = "db.example", invalidCA }, "invalid CA", "validate CA certificate"},
		{func(p *profile.Profile) { p.DBInstance = "example" }, "managed CA reaches RDS discovery", "discover database"},
		{func(p *profile.Profile) { p.DBCluster = "example" }, "managed CA reaches cluster discovery", "DescribeDBClusters"},
		{func(p *profile.Profile) { p.Host, p.RootCert = "db.example", ca }, "explicit host reaches SSM", "open tunnel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.edit(&p)
			path := filepath.Join(t.TempDir(), "profiles.json")
			require.NoError(t, profile.Save(path, "test", &p))
			for _, mode := range []string{"connect", "check"} {
				var stdout, stderr bytes.Buffer
				err := RunContext(t.Context(), []string{mode, "--config", path, "test"}, &stdout, &stderr)
				require.ErrorContains(t, err, tc.want)
				assert.Empty(t, stdout.String())
				assert.NotContains(t, stderr.String(), "Connection check passed")
			}
		})
	}
}

func TestProductionWarningBeforeConnection(t *testing.T) {
	isolateAWS(t)
	for _, environment := range []string{"production", "staging", "development", ""} {
		path := filepath.Join(t.TempDir(), "config.json")
		p := profile.Profile{Environment: environment, DBInstance: "example", Database: "data", User: "reader", Target: "i-example", Port: 5432, AWSProfile: "missing-test-profile"}
		require.NoError(t, profile.Save(path, "test", &p))
		var output bytes.Buffer
		err := RunContext(t.Context(), []string{"connect", "--config", path, "test"}, io.Discard, &output)
		require.ErrorContains(t, err, `load AWS profile "missing-test-profile"; check it exists (aws configure list-profiles)`)
		assert.Equal(t, environment == "production", strings.Contains(output.String(), "WARNING: PRODUCTION"))
	}
}

func TestCleanupRemovesAbandonedSessions(t *testing.T) {
	isolateAWS(t)
	root, err := sessionRoot()
	require.NoError(t, err)
	abandoned := filepath.Join(root, "session-abandoned")
	require.NoError(t, os.MkdirAll(abandoned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "pgpass"), []byte("secret"), 0o600))
	require.ErrorIs(t, RunContext(t.Context(), []string{"cleanup", "extra"}, io.Discard, io.Discard), ErrUsage)
	var summary bytes.Buffer
	require.NoError(t, RunContext(t.Context(), []string{"cleanup"}, io.Discard, &summary))
	assert.Equal(t, "Removed 1 abandoned session(s).\n", summary.String())
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func testCA(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	return path
}

// isolateAWS keeps the developer's AWS configuration, credentials, and caches out of a test.
func isolateAWS(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home+"/cache")
	t.Setenv("AWS_CONFIG_FILE", home+"/aws-config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", home+"/aws-credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, name := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_CREDENTIAL_EXPIRATION", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ENDPOINT_URL", "AWS_MAX_ATTEMPTS"} {
		t.Setenv(name, "")
	}
	require.NoError(t, os.WriteFile(home+"/aws-config", nil, 0o600))
}
