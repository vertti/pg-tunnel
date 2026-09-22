package awsdb_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/awsdb"
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

type fakeSSM struct{ terminated string }

// StartSession returns synthetic session details without contacting AWS.
func (*fakeSSM) StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	return &ssm.StartSessionOutput{SessionId: aws.String("session-example"), TokenValue: aws.String("sensitive-token"), StreamUrl: aws.String("wss://example.invalid")}, nil
}

// TerminateSession records remote cleanup.
func (f *fakeSSM) TerminateSession(_ context.Context, input *ssm.TerminateSessionInput, _ ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error) {
	f.terminated = aws.ToString(input.SessionId)
	return &ssm.TerminateSessionOutput{}, nil
}

func TestPluginFailureTerminatesRemoteSession(t *testing.T) {
	t.Parallel()
	plugin := filepath.Join(t.TempDir(), "plugin")
	require.NoError(t, os.WriteFile(plugin, []byte("#!/bin/sh\nprintf 'sensitive-token\\n'\nexit 7\n"), 0o700)) //nolint:gosec // The fake plugin must be executable by the test owner.
	api := &fakeSSM{}
	transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example", Plugin: plugin}
	_, err := transport.Open(t.Context(), session.Target{Host: "db.example", Port: 5432})
	require.ErrorContains(t, err, "session-manager-plugin exited")
	assert.NotContains(t, err.Error(), "sensitive-token")
	assert.Equal(t, "session-example", api.terminated)
}
