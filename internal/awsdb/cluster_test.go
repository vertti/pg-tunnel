package awsdb_test

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/profile"
)

type clusterRDS struct {
	rdsFunc
	describe func(context.Context, *rds.DescribeDBClustersInput) (*rds.DescribeDBClustersOutput, error)
}

// DescribeDBClusters supplies the cluster lookup separately from instance discovery.
func (c clusterRDS) DescribeDBClusters(ctx context.Context, input *rds.DescribeDBClustersInput, _ ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	return c.describe(ctx, input)
}

type endpointSSM struct {
	input *ssm.StartSessionInput
	fakeSSM
}

// StartSession records forwarding identity and stops before opening a remote session.
func (s *endpointSSM) StartSession(_ context.Context, input *ssm.StartSessionInput, _ ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	s.input = input
	return nil, errors.New("fixture stops before starting a remote session")
}

func TestClusterEndpointFlowsToForwardingSigningAndTLS(t *testing.T) {
	t.Parallel()
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	certServer.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certServer.Certificate().Raw}), 0o600))
	for _, tc := range []struct{ kind, host string }{
		{"", "writer.cluster.example"}, {profile.ClusterWriter, "writer.cluster.example"}, {profile.ClusterReader, "reader.cluster.example"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			api := clusterRDS{
				rdsFunc: func(context.Context, *rds.DescribeDBInstancesInput) (*rds.DescribeDBInstancesOutput, error) {
					t.Error("cluster configuration must not discover an instance")
					return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{Endpoint: &rdstypes.Endpoint{Address: aws.String("instance.example"), Port: aws.Int32(5432)}}}}, nil
				},
				describe: func(_ context.Context, input *rds.DescribeDBClustersInput) (*rds.DescribeDBClustersOutput, error) {
					assert.Equal(t, "analytics", aws.ToString(input.DBClusterIdentifier))
					return &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{Engine: aws.String("aurora-postgresql"), IAMDatabaseAuthenticationEnabled: aws.Bool(true), Endpoint: aws.String("writer.cluster.example"), ReaderEndpoint: aws.String("reader.cluster.example"), Port: aws.Int32(6432)}}}, nil
				},
			}
			p := profile.Profile{DBCluster: "analytics", ClusterEndpoint: tc.kind, Database: "data", User: "reader", RootCert: ca, Port: 5432}
			target, err := (&awsdb.Resolver{API: api, Profile: &p}).Resolve(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.host, target.Host)
			assert.Equal(t, 6432, target.Port)
			auth := awsdb.IAM{Provider: credentials.NewStaticCredentialsProvider("test", "test", ""), Region: "eu-central-1"}
			token, err := auth.Credential(t.Context(), target)
			require.NoError(t, err)
			signed, err := url.Parse("https://" + token.Secret)
			require.NoError(t, err)
			assert.Equal(t, tc.host+":6432", signed.Host)
			tls, err := libpq.TLSConfig(target)
			require.NoError(t, err)
			assert.Equal(t, tc.host, tls.ServerName)
			assert.False(t, tls.InsecureSkipVerify)
			settings, err := (libpq.Files{Root: filepath.Join(t.TempDir(), "sessions")}).Prepare(target, 12345, token)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, settings.Close()) })
			env := make(map[string]string)
			for _, entry := range settings.Env(nil) {
				key, value, _ := strings.Cut(entry, "=")
				env[key] = value
			}
			require.NotEmpty(t, env["PGSERVICEFILE"])
			data, readErr := os.ReadFile(env["PGSERVICEFILE"])
			require.NoError(t, readErr)
			assert.Contains(t, string(data), "host="+tc.host+"\n")
			assert.Contains(t, string(data), "hostaddr=127.0.0.1\n")
			assert.Contains(t, string(data), "sslmode=verify-full\n")
			ssmAPI := &endpointSSM{}
			_, err = (&awsdb.SSM{API: ssmAPI, Target: "i-test"}).Open(t.Context(), target)
			require.ErrorContains(t, err, "fixture stops")
			require.NotNil(t, ssmAPI.input)
			assert.Equal(t, []string{tc.host}, ssmAPI.input.Parameters["host"])
			assert.Equal(t, []string{"6432"}, ssmAPI.input.Parameters["portNumber"])
		})
	}
}

func TestClusterLookupRefreshesOnEachNewResolve(t *testing.T) {
	t.Parallel()
	calls := 0
	api := clusterRDS{describe: func(context.Context, *rds.DescribeDBClustersInput) (*rds.DescribeDBClustersOutput, error) {
		calls++
		return &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{Engine: aws.String("aurora-postgresql"), IAMDatabaseAuthenticationEnabled: aws.Bool(true), Endpoint: aws.String(fmt.Sprintf("writer-%d.example", calls)), Port: aws.Int32(5432)}}}, nil
	}}
	resolver := awsdb.Resolver{API: api, Profile: &profile.Profile{DBCluster: "analytics"}}
	first, err := resolver.Resolve(t.Context())
	require.NoError(t, err)
	second, err := resolver.Resolve(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "writer-1.example", first.Host)
	assert.Equal(t, "writer-2.example", second.Host)
}

func TestClusterRejectsUnusableDiscovery(t *testing.T) {
	t.Parallel()
	valid := rdstypes.DBCluster{Engine: aws.String("aurora-postgresql"), IAMDatabaseAuthenticationEnabled: aws.Bool(true), Endpoint: aws.String("writer.example"), ReaderEndpoint: aws.String("reader.example"), Port: aws.Int32(5432)}
	for _, tc := range []struct {
		failure                error
		edit                   func(*rdstypes.DBCluster)
		name, kind, auth, want string
		count                  int
	}{
		{name: "missing", count: 0, want: "found 0"},
		{name: "ambiguous", count: 2, want: "found 2"},
		{name: "denied", failure: errors.New("denied"), want: "rds:DescribeDBClusters permission"},
		{name: "wrong engine", count: 1, edit: func(c *rdstypes.DBCluster) { c.Engine = aws.String("aurora-mysql") }, want: "only Aurora PostgreSQL"},
		{name: "IAM disabled", count: 1, edit: func(c *rdstypes.DBCluster) { c.IAMDatabaseAuthenticationEnabled = nil }, want: "IAM database authentication is disabled"},
		{name: "password does not require IAM", count: 1, auth: profile.AuthSecretsManager, edit: func(c *rdstypes.DBCluster) { c.IAMDatabaseAuthenticationEnabled = nil }},
		{name: "no writer", count: 1, edit: func(c *rdstypes.DBCluster) { c.Endpoint = nil }, want: "usable writer endpoint"},
		{name: "no reader never falls back", count: 1, kind: profile.ClusterReader, edit: func(c *rdstypes.DBCluster) { c.ReaderEndpoint = nil }, want: "usable reader endpoint"},
		{name: "no port", count: 1, edit: func(c *rdstypes.DBCluster) { c.Port = nil }, want: "usable writer endpoint"},
		{name: "invalid port", count: 1, edit: func(c *rdstypes.DBCluster) { c.Port = aws.Int32(65536) }, want: "usable writer endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := valid
			if tc.edit != nil {
				tc.edit(&db)
			}
			api := clusterRDS{describe: func(context.Context, *rds.DescribeDBClustersInput) (*rds.DescribeDBClustersOutput, error) {
				return &rds.DescribeDBClustersOutput{DBClusters: slices.Repeat([]rdstypes.DBCluster{db}, tc.count)}, tc.failure
			}}
			_, err := (&awsdb.Resolver{API: api, Profile: &profile.Profile{DBCluster: "analytics", ClusterEndpoint: tc.kind, Auth: tc.auth}}).Resolve(t.Context())
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
