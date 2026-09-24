package awsdb_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
)

type rdsFunc func(context.Context, *rds.DescribeDBInstancesInput) (*rds.DescribeDBInstancesOutput, error)

// DescribeDBInstances supplies a fake RDS response.
func (f rdsFunc) DescribeDBInstances(ctx context.Context, input *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	return f(ctx, input)
}

func TestRDSDiscovery(t *testing.T) {
	t.Parallel()
	api := rdsFunc(func(_ context.Context, input *rds.DescribeDBInstancesInput) (*rds.DescribeDBInstancesOutput, error) {
		assert.Equal(t, "example", aws.ToString(input.DBInstanceIdentifier))
		return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{Engine: aws.String("postgres"), IAMDatabaseAuthenticationEnabled: aws.Bool(true), Endpoint: &rdstypes.Endpoint{Address: aws.String("real.rds.amazonaws.com"), Port: aws.Int32(5432)}}}}, nil
	})
	r := awsdb.Resolver{API: api, Profile: &profile.Profile{DBInstance: "example", Database: "data", User: "reader"}}
	value, err := r.Resolve(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "real.rds.amazonaws.com", value.Host)
	assert.Equal(t, 5432, value.Port)
	assert.Equal(t, "reader", value.User)
}

type ec2Func func(context.Context, *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)

// DescribeInstances supplies fake paginated jump-host discovery.
func (f ec2Func) DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return f(ctx, input)
}

func TestJumpHostRejectsAmbiguityAcrossPages(t *testing.T) {
	t.Parallel()
	api := ec2Func(func(_ context.Context, input *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		output := &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{InstanceId: aws.String("i-example")}}}}}
		if input.NextToken == nil {
			output.NextToken = aws.String("next")
		}
		return output, nil
	})
	_, err := awsdb.JumpHost(t.Context(), api, &profile.Profile{JumpTag: "jump"})
	require.ErrorContains(t, err, "found 2")
}

func TestIAMSignsRemoteEndpointAndReportsExpiry(t *testing.T) {
	t.Parallel()
	provider := credentials.NewStaticCredentialsProvider("test-access", "test-secret", "test-session")
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	auth := awsdb.IAM{Provider: providerFunc(func(ctx context.Context) (aws.Credentials, error) {
		value, err := provider.Retrieve(ctx)
		require.NoError(t, err)
		value.Source = "EnvConfigCredentials"
		return value, nil
	}), Region: "eu-central-1", EnvironmentExpiry: expiry}
	value, err := auth.Credential(t.Context(), session.Target{Host: "db.rds.amazonaws.com", Port: 5432, User: "reader"})
	require.NoError(t, err)
	parsed, err := url.Parse("https://" + value.Secret)
	require.NoError(t, err)
	assert.Equal(t, "db.rds.amazonaws.com:5432", parsed.Host)
	assert.Equal(t, "reader", parsed.Query().Get("DBUser"))
	assert.Equal(t, "900", parsed.Query().Get("X-Amz-Expires"))
	assert.Equal(t, expiry, value.ExpiresAt)
	auth.EnvironmentExpiry = time.Now().Add(-time.Second)
	_, err = auth.Credential(t.Context(), session.Target{})
	require.ErrorContains(t, err, "renew the AWS session")
}

type providerFunc func(context.Context) (aws.Credentials, error)

// Retrieve supplies renewable AWS credentials.
func (f providerFunc) Retrieve(ctx context.Context) (aws.Credentials, error) { return f(ctx) }

func TestIAMRetrievesCredentialsOnEveryRefresh(t *testing.T) {
	t.Parallel()
	calls := 0
	failure := errors.New("SSO session expired")
	auth := awsdb.IAM{Region: "eu-central-1", Provider: providerFunc(func(context.Context) (aws.Credentials, error) {
		calls++
		if calls == 2 {
			return aws.Credentials{}, failure
		}
		return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
	})}
	_, err := auth.Credential(t.Context(), session.Target{Host: "db.example", Port: 5432})
	require.NoError(t, err)
	_, err = auth.Credential(t.Context(), session.Target{Host: "db.example", Port: 5432})
	require.ErrorIs(t, err, failure)
	assert.Equal(t, 2, calls)
}

type fakeSSM struct {
	historyInput   *ssm.DescribeSessionsInput
	terminationErr error
	historyErr     error
	terminated     string
	history        []types.Session
}

// DescribeSessions returns the configured remote cleanup outcome.
func (f *fakeSSM) DescribeSessions(_ context.Context, input *ssm.DescribeSessionsInput, _ ...func(*ssm.Options)) (*ssm.DescribeSessionsOutput, error) {
	f.historyInput = input
	return &ssm.DescribeSessionsOutput{Sessions: f.history}, f.historyErr
}

// StartSession returns synthetic session details without contacting AWS.
func (*fakeSSM) StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	return &ssm.StartSessionOutput{SessionId: aws.String("session-example"), TokenValue: aws.String("sensitive-token"), StreamUrl: aws.String("wss://example.invalid")}, nil
}

// TerminateSession records remote cleanup.
func (f *fakeSSM) TerminateSession(_ context.Context, input *ssm.TerminateSessionInput, _ ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error) {
	f.terminated = aws.ToString(input.SessionId)
	return &ssm.TerminateSessionOutput{}, f.terminationErr
}

func TestPluginFailureTerminatesRemoteSession(t *testing.T) {
	t.Parallel()
	plugin := filepath.Join(t.TempDir(), "plugin")
	require.NoError(t, os.WriteFile(plugin, []byte("#!/bin/sh\nprintf 'sensitive-token\\n'\nexit 7\n"), 0o700)) //nolint:gosec // The fake plugin must be executable by the test owner.
	api := &fakeSSM{}
	transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example", Executable: plugin}
	_, err := transport.Open(t.Context(), session.Target{Host: "db.example", Port: 5432})
	require.ErrorContains(t, err, "embedded SSM child exited")
	assert.Equal(t, 1, process.ExitCode(err), "the plugin's status must not look like the user's command status")
	assert.NotContains(t, err.Error(), "sensitive-token")
	assert.Equal(t, "session-example", api.terminated)
}

func TestIAMRefreshesCachedAWSCredentialsAfterProviderRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		failure := errors.New("credential provider temporarily unavailable")
		cache := aws.NewCredentialsCache(providerFunc(func(context.Context) (aws.Credentials, error) {
			calls++
			if calls == 2 {
				return aws.Credentials{}, failure
			}
			return aws.Credentials{AccessKeyID: fmt.Sprintf("access-%d", calls), SecretAccessKey: "test-secret", SessionToken: "test-session", CanExpire: true, Expires: time.Now().Add(5 * time.Minute)}, nil
		}), func(options *aws.CredentialsCacheOptions) { options.ExpiryWindow = 2 * time.Minute })
		auth := awsdb.IAM{Region: "eu-central-1", Provider: cache}
		target := session.Target{Host: "db.example", Port: 5432, User: "reader"}
		first, err := auth.Credential(t.Context(), target)
		require.NoError(t, err)
		assert.Contains(t, first.Secret, "X-Amz-Credential=access-1%2F")
		time.Sleep(4 * time.Minute)
		_, err = auth.Credential(t.Context(), target)
		require.ErrorIs(t, err, failure)
		replacement, err := auth.Credential(t.Context(), target)
		require.NoError(t, err)
		assert.Contains(t, replacement.Secret, "X-Amz-Credential=access-3%2F")
		assert.True(t, replacement.ExpiresAt.After(first.ExpiresAt))
	})
}

func TestRemoteCleanupConfirmsFailedTermination(t *testing.T) {
	t.Parallel()
	failure := errors.New("termination rejected")
	denied := errors.New("history access denied")
	for _, test := range []struct {
		terminationErr error
		historyErr     error
		name           string
		id             string
		status         types.SessionStatus
		wantFailure    bool
	}{
		{name: "accepted"},
		{name: "already terminated", terminationErr: failure, id: "session-example", status: types.SessionStatusTerminated},
		{name: "still terminating", terminationErr: failure, id: "session-example", status: types.SessionStatusTerminating, wantFailure: true},
		{name: "different session", terminationErr: failure, id: "other", status: types.SessionStatusTerminated, wantFailure: true},
		{name: "no history", terminationErr: failure, wantFailure: true},
		{name: "history denied", terminationErr: failure, historyErr: denied, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeSSM{terminationErr: test.terminationErr, historyErr: test.historyErr}
			if test.id != "" {
				api.history = []types.Session{{SessionId: aws.String(test.id), Status: test.status}}
			}
			transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example", Executable: filepath.Join(t.TempDir(), "missing")}
			_, err := transport.Open(t.Context(), session.Target{Host: "db.example", Port: 5432})
			require.ErrorContains(t, err, "launch embedded SSM child")
			if test.wantFailure {
				require.ErrorIs(t, err, failure)
			} else {
				require.NotErrorIs(t, err, failure)
			}
			if test.historyErr != nil {
				require.ErrorIs(t, err, denied)
			}
			if test.terminationErr == nil {
				assert.Nil(t, api.historyInput)
			} else {
				require.NotNil(t, api.historyInput)
				assert.Equal(t, types.SessionStateHistory, api.historyInput.State)
				assert.Equal(t, []types.SessionFilter{{Key: types.SessionFilterKeySessionId, Value: aws.String("session-example")}}, api.historyInput.Filters)
			}
		})
	}
}

func TestPasswordAuthenticationDoesNotRequireRDSIAM(t *testing.T) {
	t.Parallel()
	api := rdsFunc(func(context.Context, *rds.DescribeDBInstancesInput) (*rds.DescribeDBInstancesOutput, error) {
		return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{Engine: aws.String("postgres"), IAMDatabaseAuthenticationEnabled: aws.Bool(false), Endpoint: &rdstypes.Endpoint{Address: aws.String("db.example"), Port: aws.Int32(5432)}}}}, nil
	})
	p := profile.Profile{DBInstance: "example", Database: "data", User: "reader"}
	resolver := awsdb.Resolver{API: api, Profile: &p}
	_, err := resolver.Resolve(t.Context())
	require.ErrorContains(t, err, "IAM")
	p.Auth, p.SecretID = profile.AuthSecretsManager, "chosen-secret"
	target, err := resolver.Resolve(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "db.example", target.Host)
}

func TestIAMReportsCredentialExpiryKnowledge(t *testing.T) {
	t.Parallel()
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name, want string
		value      aws.Credentials
	}{
		{"expiring", "AWS credentials expire " + expiry.Format(time.RFC3339), aws.Credentials{CanExpire: true, Expires: expiry}},
		{"unknown temporary", "static environment credentials cannot be renewed", aws.Credentials{SessionToken: "token"}},
		{"long-term", "expiry is not reported", aws.Credentials{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value := tc.value
			value.AccessKeyID, value.SecretAccessKey = "test", "test"
			var reported []string
			auth := awsdb.IAM{Region: "eu-central-1", Report: func(message string) { reported = append(reported, message) }, Provider: providerFunc(func(context.Context) (aws.Credentials, error) { return value, nil })}
			_, err := auth.Credential(t.Context(), session.Target{Host: "db.example", Port: 5432, User: "reader"})
			require.NoError(t, err)
			require.Len(t, reported, 1)
			assert.Contains(t, reported[0], tc.want)
		})
	}
}

func TestRDSDiscoveryRejectsUnusableInstances(t *testing.T) {
	t.Parallel()
	address := aws.String("real.rds.amazonaws.com")
	for _, tc := range []struct {
		err        error
		name, want string
		instances  []rdstypes.DBInstance
	}{
		{name: "API failure", err: errors.New("AccessDenied"), want: "rds:DescribeDBInstances permission"},
		{name: "no instance", want: "found 0"},
		{name: "other engine", instances: []rdstypes.DBInstance{{Engine: aws.String("mysql")}}, want: "RDS PostgreSQL"},
		{name: "no endpoint", instances: []rdstypes.DBInstance{{Engine: aws.String("postgres"), IAMDatabaseAuthenticationEnabled: aws.Bool(true)}}, want: "usable database endpoint"},
		{name: "endpoint without port", instances: []rdstypes.DBInstance{{Engine: aws.String("postgres"), IAMDatabaseAuthenticationEnabled: aws.Bool(true), Endpoint: &rdstypes.Endpoint{Address: address}}}, want: "usable database endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := rdsFunc(func(context.Context, *rds.DescribeDBInstancesInput) (*rds.DescribeDBInstancesOutput, error) {
				return &rds.DescribeDBInstancesOutput{DBInstances: tc.instances}, tc.err
			})
			r := awsdb.Resolver{API: api, Profile: &profile.Profile{DBInstance: "example"}}
			_, err := r.Resolve(t.Context())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestExplicitHostSkipsRDSDiscovery(t *testing.T) {
	t.Parallel()
	r := awsdb.Resolver{Profile: &profile.Profile{Host: "db.example", Port: 6432, Database: "data", User: "reader"}}
	target, err := r.Resolve(t.Context())
	require.NoError(t, err)
	assert.Equal(t, session.Target{Host: "db.example", Port: 6432, Database: "data", User: "reader"}, target)
}
