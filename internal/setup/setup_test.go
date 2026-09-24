package setup_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/setup"
)

type httpFunc func(*http.Request) (*http.Response, error)

func (f httpFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func fixtureConfig(t *testing.T, denied string) aws.Config {
	t.Helper()
	return aws.Config{Region: "eu-central-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1, HTTPClient: httpFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, request.Body.Close())
		var content string
		service := strings.Split(request.URL.Hostname(), ".")[0]
		switch service {
		case "sts":
			content = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`
		case "rds":
			content = rdsPage(string(body))
		case "ssm":
			assert.Equal(t, "AmazonSSM.DescribeInstanceInformation", request.Header.Get("X-Amz-Target"))
			assert.Contains(t, string(body), `"Online"`)
			content = `{"InstanceInformationList":[{"InstanceId":"i-other","PingStatus":"Online"}],"NextToken":"next"}`
			if strings.Contains(string(body), `"NextToken":"next"`) {
				content = `{"InstanceInformationList":[{"InstanceId":"i-chosen","PingStatus":"Online"}]}`
			}
		case "ec2":
			content = ec2Page(string(body))
		default:
			t.Errorf("unexpected AWS service: %s", service)
		}
		status := http.StatusOK
		if service == denied {
			status = http.StatusForbidden
			content = `{"__type":"AccessDeniedException","Message":"not authorized"}`
			if service != "ssm" {
				content = `<Response><Errors><Error><Code>AccessDenied</Code><Message>not authorized</Message></Error></Errors></Response>`
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(content)), Request: request}, nil
	})}
}

func rdsPage(body string) string {
	contents := `<DBInstances><DBInstance><DBInstanceIdentifier>mysql</DBInstanceIdentifier><Engine>mysql</Engine></DBInstance></DBInstances><Marker>next</Marker>`
	if strings.Contains(body, "Marker=next") {
		contents = `<DBInstances><DBInstance><DBInstanceIdentifier>example</DBInstanceIdentifier><Engine>postgres</Engine><DBInstanceStatus>available</DBInstanceStatus><IAMDatabaseAuthenticationEnabled>true</IAMDatabaseAuthenticationEnabled><DBName>data</DBName><MasterUsername>admin</MasterUsername><Endpoint><Address>example.rds.amazonaws.com</Address><Port>5433</Port></Endpoint><DBSubnetGroup><VpcId>vpc-db</VpcId></DBSubnetGroup><MasterUserSecret><SecretArn>arn:aws:secretsmanager:eu-central-1:123456789012:secret:master</SecretArn></MasterUserSecret></DBInstance></DBInstances>`
	}
	return `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><DescribeDBInstancesResult>` + contents + `</DescribeDBInstancesResult></DescribeDBInstancesResponse>`
}

func ec2Page(body string) string {
	contents := `<reservationSet><item><instancesSet><item><instanceId>i-other</instanceId><vpcId>vpc-other</vpcId></item></instancesSet></item></reservationSet><nextToken>next</nextToken>`
	if strings.Contains(body, "NextToken=next") {
		contents = `<reservationSet><item><instancesSet><item><instanceId>i-chosen</instanceId><vpcId>vpc-db</vpcId><tagSet><item><key>Name</key><value>jump</value></item></tagSet></item><item><instanceId>i-offline</instanceId><vpcId>vpc-db</vpcId></item></instancesSet></item></reservationSet>`
	}
	return `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` + contents + `</DescribeInstancesResponse>`
}

func certificate(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	return path
}

func TestWizardDiscoversAcrossPagesAndSavesOnlyAfterConfirmation(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	for _, answer := range []string{"yes", "no", "", "EOF"} {
		t.Run(answer, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "profiles.json")
			var output bytes.Buffer
			// Invalid selection is retried; database name defaults to the RDS hint.
			input := "bad\n9\n1\n1\n1\n\nreader\nreadonly\n"
			if answer != "EOF" {
				input += answer + "\n"
			}
			wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, ""), Input: strings.NewReader(input), Output: &output, Path: path, RootCert: ca, AWSProfile: "dev"}
			verified := false
			wizard.Verify = func(context.Context, *profile.Profile) error { verified = true; return nil }
			err := wizard.Run(t.Context())
			if answer == "EOF" {
				require.ErrorContains(t, err, "input ended")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, answer == "yes", verified, "verification requires confirmation")
			assert.Contains(t, output.String(), "123456789012")
			assert.Contains(t, output.String(), "RDS-linked master-user secret")
			assert.NotContains(t, output.String(), "i-offline")
			assert.NotContains(t, output.String(), "mysql")
			if answer != "yes" {
				assert.NoFileExists(t, path)
				return
			}
			saved, err := profile.Load(path, "readonly")
			require.NoError(t, err)
			assert.Equal(t, "i-chosen", saved.Target, "same-VPC host on the second page is ranked first")
			assert.Equal(t, "reader", saved.User, "never default to the RDS master user")
			assert.Equal(t, "data", saved.Database)
			assert.Equal(t, 5433, saved.Port)
			assert.Equal(t, ca, saved.RootCert)
			assert.Equal(t, "dev", saved.AWSProfile)
			contents, err := os.ReadFile(path) //nolint:gosec // The path is inside t.TempDir.
			require.NoError(t, err)
			assert.NotContains(t, string(contents), "secret:master")
		})
	}
}

func TestWizardDiscoveryPermissionFailures(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	for _, service := range []string{"sts", "rds", "ssm", "ec2"} {
		t.Run(service, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "profiles.json")
			var output bytes.Buffer
			wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, service), Input: strings.NewReader("1\n1\ni-manual\n1\ndata\nreader\nreadonly\nyes\n"), Output: &output, Path: path, RootCert: ca}
			err := wizard.Run(t.Context())
			if service == "sts" || service == "rds" {
				require.Error(t, err)
				assert.NoFileExists(t, path)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, output.String(), "Jump-host discovery unavailable")
			saved, err := profile.Load(path, "readonly")
			require.NoError(t, err)
			assert.Equal(t, "i-manual", saved.Target)
		})
	}
}

func TestWizardCancellationAndOutputFailureWriteNothing(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{true, false} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			path := filepath.Join(t.TempDir(), "profiles.json")
			wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, ""), Input: strings.NewReader(""), Output: io.Discard, Path: path}
			if !cancelled {
				wizard.Output = brokenWriter{}
			}
			require.Error(t, wizard.Run(ctx))
			assert.NoFileExists(t, path)
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWizardRejectsUnusableDatabaseAndCA(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, from, to, want string }{
		{"no endpoint", "<Address>example.rds.amazonaws.com</Address>", "<Address></Address>", "no endpoint yet"},
		{"no PostgreSQL", "<Engine>postgres</Engine>", "<Engine>mysql</Engine>", "no RDS PostgreSQL instances"},
		{"missing CA", "unchanged", "unchanged", "validate CA bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := fixtureConfig(t, "")
			client := cfg.HTTPClient
			cfg.HTTPClient = httpFunc(func(request *http.Request) (*http.Response, error) {
				response, err := client.Do(request)
				if err != nil {
					return nil, fmt.Errorf("fixture response: %w", err)
				}
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				response.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(body), tc.from, tc.to)))
				return response, nil
			})
			path := filepath.Join(t.TempDir(), "profiles.json")
			input := "1\n1\n1\ndata\nreader\n"
			wizard := setup.Wizard{Verify: successfulVerification, Config: cfg, Input: strings.NewReader(input), Output: io.Discard, Path: path, RootCert: filepath.Join(t.TempDir(), "missing.pem")}
			require.ErrorContains(t, wizard.Run(t.Context()), tc.want)
			assert.NoFileExists(t, path)
		})
	}
}

func successfulVerification(context.Context, *profile.Profile) error { return nil }

func TestWizardOnlySavesAfterSuccessfulVerification(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	for _, result := range []string{"success", "login failure", "cleanup failure", "cancelled"} {
		t.Run(result, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "profiles.json")
			original := profile.Profile{DBInstance: "existing", Database: "data", User: "reader", Target: "i-existing", RootCert: ca, Port: 5432}
			require.NoError(t, profile.Save(path, "existing", &original))
			before, err := os.ReadFile(path) //nolint:gosec // The configuration is inside t.TempDir.
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New(result)
			var output bytes.Buffer
			wizard := setup.Wizard{Config: fixtureConfig(t, ""), Input: strings.NewReader("1\n1\n1\ndata\nreader\nreadonly\nyes\n"), Output: &output, Path: path, RootCert: ca}
			wizard.Verify = func(_ context.Context, candidate *profile.Profile) error {
				// The new profile is not on disk while the test session is running.
				current, readErr := os.ReadFile(path) //nolint:gosec // The configuration is inside t.TempDir.
				require.NoError(t, readErr)
				assert.Equal(t, before, current)
				assert.Equal(t, "i-chosen", candidate.Target)
				assert.Equal(t, "reader", candidate.User)
				candidate.RootCert = "/temporary/resolved-ca.pem"
				switch result {
				case "success":
					return nil
				case "cancelled":
					cancel()
					return nil
				default:
					return failure
				}
			}
			err = wizard.Run(ctx)
			if result == "success" {
				require.NoError(t, err)
				saved, loadErr := profile.Load(path, "readonly")
				require.NoError(t, loadErr)
				assert.Equal(t, ca, saved.RootCert, "verification must not change saved settings")
				assert.Contains(t, output.String(), "Saved verified connection")
				assert.NotContains(t, output.String(), "Verify access with")
				return
			}
			if result == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, failure)
				require.ErrorContains(t, err, "configuration was not saved")
			}
			after, readErr := os.ReadFile(path) //nolint:gosec // The configuration is inside t.TempDir.
			require.NoError(t, readErr)
			assert.Equal(t, before, after)
			assert.NotContains(t, output.String(), "Saved verified connection")
		})
	}
}

func TestWizardDefaultsToUniqueResources(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	for _, tc := range []struct {
		name, input, target, prompt string
		singleHost                  bool
	}{
		{"one host", "\n\n\n\n\nreader\nreadonly\nyes\n", "i-chosen", "Jump host number [1]:", true},
		{"multiple hosts", "\n\n2\n\n\n\nreader\nreadonly\nyes\n", "i-other", "Jump host number:", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := fixtureConfig(t, "")
			client := cfg.HTTPClient
			cfg.HTTPClient = httpFunc(func(request *http.Request) (*http.Response, error) {
				if tc.singleHost && request.Header.Get("X-Amz-Target") == "AmazonSSM.DescribeInstanceInformation" {
					require.NoError(t, request.Body.Close())
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"InstanceInformationList":[{"InstanceId":"i-chosen","PingStatus":"Online"}]}`)), Request: request}, nil
				}
				response, err := client.Do(request)
				if err != nil {
					return nil, fmt.Errorf("fixture response: %w", err)
				}
				return response, nil
			})

			var output bytes.Buffer
			path := filepath.Join(t.TempDir(), "profiles.json")
			wizard := setup.Wizard{Verify: successfulVerification, Config: cfg, Input: strings.NewReader(tc.input), Output: &output, Path: path, RootCert: ca}
			require.NoError(t, wizard.Run(t.Context()))
			saved, err := profile.Load(path, "readonly")
			require.NoError(t, err)
			assert.Equal(t, tc.target, saved.Target)
			assert.Contains(t, output.String(), "Database number [1]:")
			assert.NotContains(t, output.String(), `[""]`)
			assert.Contains(t, output.String(), tc.prompt)
			assert.Contains(t, output.String(), "IAM database user (must already exist with rds_iam membership): ")
			assert.Contains(t, output.String(), "A value is required.")
			assert.Equal(t, "reader", saved.User)
		})
	}
}

func TestWizardPasswordAuthentication(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input, secret, user string
		disableIAM, cancel        bool
	}{
		{name: "linked master", input: "2\n\n\n\n", secret: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:master", user: "admin"}, //nolint:gosec // Synthetic secret ARN, not a credential.
		{name: "custom secret", input: "2\napplication/reader\n\nreader\n", secret: "application/reader", user: "reader"},
		{name: "IAM disabled", input: "\n\n\n\n", secret: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:master", user: "admin", disableIAM: true}, //nolint:gosec // Synthetic secret ARN, not a credential.
		{name: "cancelled", input: "2\napplication/reader\n\nreader\n", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := fixtureConfig(t, "")
			client := cfg.HTTPClient
			cfg.HTTPClient = httpFunc(func(request *http.Request) (*http.Response, error) {
				response, err := client.Do(request)
				if err != nil {
					return nil, fmt.Errorf("fixture response: %w", err)
				}
				if tc.disableIAM {
					body, readErr := io.ReadAll(response.Body)
					require.NoError(t, readErr)
					require.NoError(t, response.Body.Close())
					response.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(body), "<IAMDatabaseAuthenticationEnabled>true", "<IAMDatabaseAuthenticationEnabled>false")))
				}
				return response, nil
			})
			answer := "yes"
			if tc.cancel {
				answer = "no"
			}
			var output bytes.Buffer
			path := filepath.Join(t.TempDir(), "profiles.json")
			verified := false
			wizard := setup.Wizard{Config: cfg, Input: strings.NewReader("1\n1\n" + tc.input + "password-connection\n" + answer + "\n"), Output: &output, Path: path, RootCert: certificate(t)}
			wizard.Verify = func(_ context.Context, p *profile.Profile) error {
				verified = true
				assert.Equal(t, profile.AuthSecretsManager, p.Auth)
				assert.Equal(t, tc.secret, p.SecretID)
				assert.Equal(t, tc.user, p.User)
				assert.Contains(t, output.String(), "Test and save this connection?")
				return nil
			}
			require.NoError(t, wizard.Run(t.Context()))
			assert.Equal(t, !tc.cancel, verified)
			if tc.cancel {
				assert.NoFileExists(t, path)
				return
			}
			saved, err := profile.Load(path, "password-connection")
			require.NoError(t, err)
			assert.Equal(t, profile.AuthSecretsManager, saved.Auth)
			assert.Equal(t, tc.secret, saved.SecretID)
			assert.Equal(t, tc.user, saved.User)
		})
	}
}
