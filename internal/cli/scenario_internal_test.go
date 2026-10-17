package cli

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
	"github.com/vertti/pg-tunnel/internal/testutil"
)

// scenario runs the real session path against a fake AWS endpoint and a real
// PostgreSQL server reached without SSM.
type scenario struct {
	t        *testing.T
	postgres testutil.Postgres
	aws      *fakeAWS
	mu       sync.Mutex
	messages []string
	env      []string
	ssm      bool
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	isolateAWS(t)
	s := &scenario{t: t, postgres: testutil.StartPostgres(t), aws: &fakeAWS{t: t, password: testutil.PostgresPassword}}
	server := httptest.NewServer(s.aws)
	t.Cleanup(server.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_ENDPOINT_URL", server.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	configureRunner = func(runner *session.Runner) {
		if !s.ssm {
			runner.Transport = directTransport{port: s.postgres.Port}
		}
		runner.PasswordPoll = 200 * time.Millisecond
	}
	t.Cleanup(func() { configureRunner = func(*session.Runner) {} })
	return s
}

func (s *scenario) secretsManagerProfile() *profile.Profile {
	target := s.postgres.Target
	return &profile.Profile{Host: target.Host, Port: target.Port, Database: target.Database, User: target.User, RootCert: target.RootCert, Auth: profile.AuthSecretsManager, SecretID: "db-secret", Target: "i-scenario", Region: "eu-west-1"}
}

// start runs a connect session until the test ends and returns once its client settings are published.
func (s *scenario) start(p *profile.Profile) {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.t.Context()))
	ready := make(chan struct{})
	var command session.Command = func(ctx context.Context, env []string) error {
		s.mu.Lock()
		s.env = env
		s.mu.Unlock()
		close(ready)
		<-ctx.Done()
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- execute(ctx, p, command, s.report, false) }()
	s.t.Cleanup(func() {
		cancel()
		assert.NoError(s.t, <-result)
	})
	select {
	case <-ready:
	case err := <-result:
		s.t.Fatalf("session ended before its command started: %v\n%s", err, s.output())
	case <-time.After(30 * time.Second):
		s.t.Fatalf("session did not start\n%s", s.output())
	}
}

// fail runs a session that must stop before its command starts.
func (s *scenario) fail(p *profile.Profile) error {
	s.t.Helper()
	var command session.Command = func(context.Context, []string) error {
		s.t.Error("session command started")
		return nil
	}
	err := execute(s.t.Context(), p, command, s.report, false)
	require.Error(s.t, err)
	return err
}

func (s *scenario) report(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
}

func (s *scenario) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.messages, "\n")
}

func (s *scenario) waitForMessage(substring string) {
	s.t.Helper()
	if !assert.Eventually(s.t, func() bool { return strings.Contains(s.output(), substring) }, 20*time.Second, 50*time.Millisecond) {
		s.t.Fatalf("waiting for %q in:\n%s", substring, s.output())
	}
}

// psql connects the way a user's client would, through the session's published settings.
func (s *scenario) psql(query string) error {
	s.t.Helper()
	s.mu.Lock()
	env := s.env
	s.mu.Unlock()
	cmd := exec.CommandContext(s.t.Context(), filepath.Join(s.postgres.Bin, "psql"), "-X", "-v", "ON_ERROR_STOP=1", "-c", query)
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		return &psqlError{err: err, output: string(output)}
	}
	return nil
}

func (s *scenario) setDatabasePassword(password string) {
	s.t.Helper()
	require.NoError(s.t, s.psql("ALTER ROLE postgres PASSWORD '"+password+"'"))
}

type psqlError struct {
	err    error
	output string
}

func (e *psqlError) Error() string { return e.err.Error() + ": " + e.output }

type directTransport struct{ port int }

func (d directTransport) Open(context.Context, session.Target) (session.Tunnel, error) {
	return directTunnel{port: d.port, done: make(chan struct{})}, nil
}

type directTunnel struct {
	port int
	done chan struct{}
}

func (d directTunnel) Port() int                   { return d.port }
func (d directTunnel) Done() <-chan struct{}       { return d.done }
func (d directTunnel) Err() error                  { return nil }
func (d directTunnel) Close(context.Context) error { return nil }

// fakeAWS answers the AWS calls pg-tunnel makes, identified by IAM action.
type fakeAWS struct {
	t        *testing.T
	mu       sync.Mutex
	password string
	version  int
	failures map[string]awsFailure
}

type awsFailure struct{ code, message string }

// fail makes every call of action return an AWS error; an empty code succeeds again.
func (f *fakeAWS) fail(action, code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = map[string]awsFailure{}
	}
	f.failures[action] = awsFailure{code, message}
}

func (f *fakeAWS) deny(action string) {
	f.fail(action, "AccessDeniedException", deniedMessage(action))
}

func deniedMessage(action string) string {
	return fmt.Sprintf("User: arn:aws:sts::123456789012:assumed-role/Developer/scenario is not authorized to perform: %s because no identity-based policy allows the %s action", action, action)
}

// action names a request the way IAM does, such as rds:DescribeDBInstances.
func action(r *http.Request) string {
	scope := strings.Split(strings.SplitN(r.Header.Get("Authorization"), "Credential=", 2)[1], "/")
	service := scope[3]
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		return service + ":" + target[strings.LastIndex(target, ".")+1:]
	}
	if err := r.ParseForm(); err != nil {
		return service + ":unreadable"
	}
	return service + ":" + r.Form.Get("Action")
}

// writeAWSError answers in the error format of each service's protocol, using
// that service's code for access denied.
func writeAWSError(t *testing.T, w http.ResponseWriter, action string, failure awsFailure) {
	code, message := failure.code, failure.message
	service, _, _ := strings.Cut(action, ":")
	switch service {
	case "rds":
		if code == "AccessDeniedException" {
			code = "AccessDenied"
		}
		type rdsError struct {
			Type    string
			Code    string
			Message string
		}
		writeAWSXML(t, w, struct {
			XMLName xml.Name `xml:"ErrorResponse"`
			Error   rdsError
		}{Error: rdsError{"Sender", code, message}})
	case "ec2":
		if code == "AccessDeniedException" {
			code, message = "UnauthorizedOperation", "You are not authorized to perform this operation. "+message
		}
		type ec2Error struct {
			Code    string
			Message string
		}
		writeAWSXML(t, w, struct {
			XMLName xml.Name   `xml:"Response"`
			Errors  []ec2Error `xml:"Errors>Error"`
		}{Errors: []ec2Error{{code, message}}})
	default:
		writeAWSJSON(t, w, http.StatusBadRequest, map[string]string{"__type": code, "Message": message})
	}
}

func writeAWSXML(t *testing.T, w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusForbidden)
	assert.NoError(t, xml.NewEncoder(w).Encode(body))
}

func (f *fakeAWS) rotateSecret(password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.password = password
	f.version++
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	operation := action(r)
	f.mu.Lock()
	failure, failing := f.failures[operation]
	f.mu.Unlock()
	if failing && failure.code != "" {
		writeAWSError(f.t, w, operation, failure)
		return
	}
	switch operation {
	case "secretsmanager:GetSecretValue":
		f.mu.Lock()
		secret, err := json.Marshal(map[string]string{"username": "postgres", "password": f.password})
		assert.NoError(f.t, err)
		created := time.Date(2026, 10, 17, 12, f.version, 0, 0, time.UTC)
		f.mu.Unlock()
		writeAWSJSON(f.t, w, http.StatusOK, map[string]any{"Name": "db-secret", "SecretString": string(secret), "CreatedDate": created.Unix()})
	default:
		f.t.Errorf("fake AWS does not implement %q", operation)
		writeAWSJSON(f.t, w, http.StatusBadRequest, map[string]string{"__type": "UnknownOperationException"})
	}
}

func writeAWSJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

func TestScenarioSecretRotation(t *testing.T) {
	s := newScenario(t)
	s.start(s.secretsManagerProfile())
	require.NoError(t, s.psql("SELECT 1"))
	s.waitForMessage("Secrets Manager password loaded (created 2026-10-17T12:00:00Z)")

	s.setDatabasePassword("rotated-once")
	s.aws.rotateSecret("rotated-once")
	s.waitForMessage("Secrets Manager password rotated (created 2026-10-17T12:01:00Z)")
	require.NoError(t, s.psql("SELECT 1"), "new connections use the rotated password")

	s.aws.rotateSecret("rotated-twice")
	s.waitForMessage("previous password is still published")
	require.NoError(t, s.psql("SELECT 1"), "a password the database rejects is never published")
	s.setDatabasePassword("rotated-twice")
	s.waitForMessage("Secrets Manager password rotated (created 2026-10-17T12:02:00Z)")
	require.NoError(t, s.psql("SELECT 1"))

	assert.Equal(t, 1, strings.Count(s.output(), "Secrets Manager password loaded"), "unchanged passwords are not re-reported")
}

const deniedPrefix = "User: arn:aws:sts::123456789012:assumed-role/Developer/scenario is not authorized to perform: "

func TestScenarioAWSFailuresAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name, action, code, message string
		setup                       func(*profile.Profile)
		want                        string
	}{
		{
			name: "jump host lookup denied", action: "ec2:DescribeInstances", setup: func(p *profile.Profile) { p.Target, p.JumpTag = "", "bastion" },
			want: `discover jump host: EC2 DescribeInstances for Name="bastion" was denied: You are not authorized to perform this operation. ` + deniedPrefix + "ec2:DescribeInstances because no identity-based policy allows the ec2:DescribeInstances action. To skip this lookup, set target to the jump host's instance ID",
		},
		{
			name: "instance lookup denied", action: "rds:DescribeDBInstances", setup: func(p *profile.Profile) { p.Host, p.DBInstance = "", "app-db" },
			want: `discover database: RDS DescribeDBInstances for "app-db" in eu-west-1 was denied: ` + deniedPrefix + "rds:DescribeDBInstances because no identity-based policy allows the rds:DescribeDBInstances action. To skip discovery, set host and port on the connection",
		},
		{
			name: "cluster lookup denied", action: "rds:DescribeDBClusters", setup: func(p *profile.Profile) { p.Host, p.DBCluster = "", "app-cluster" },
			want: `discover database: RDS DescribeDBClusters for "app-cluster" in eu-west-1 was denied: ` + deniedPrefix + "rds:DescribeDBClusters because no identity-based policy allows the rds:DescribeDBClusters action. To skip discovery, set host and port on the connection",
		},
		{
			name: "session denied", action: "ssm:StartSession",
			want: "database session: open tunnel: SSM StartSession for i-scenario was denied: " + deniedPrefix + "ssm:StartSession because no identity-based policy allows the ssm:StartSession action",
		},
		{
			name: "secret denied", action: "secretsmanager:GetSecretValue",
			want: `database session: obtain database credential: Secrets Manager GetSecretValue for "db-secret" was denied (AccessDeniedException); the AWS role needs secretsmanager:GetSecretValue on the secret, and kms:Decrypt if it uses a customer-managed key`,
		},
		{
			name: "instance not found", action: "rds:DescribeDBInstances", code: "DBInstanceNotFound", message: "DBInstance app-db not found.", setup: func(p *profile.Profile) { p.Host, p.DBInstance = "", "app-db" },
			want: `discover database: RDS DescribeDBInstances for "app-db" in eu-west-1 failed: DBInstance app-db not found. Check the instance name, region and AWS profile`,
		},
		{
			name: "cluster not found", action: "rds:DescribeDBClusters", code: "DBClusterNotFoundFault", message: "DBCluster app-cluster not found.", setup: func(p *profile.Profile) { p.Host, p.DBCluster = "", "app-cluster" },
			want: `discover database: RDS DescribeDBClusters for "app-cluster" in eu-west-1 failed: DBCluster app-cluster not found. Check the cluster name, region and AWS profile`,
		},
		{
			name: "secret not found", action: "secretsmanager:GetSecretValue", code: "ResourceNotFoundException", message: "Secrets Manager can't find the specified secret.",
			want: `database session: obtain database credential: Secrets Manager secret "db-secret" not found in eu-west-1; check secret_id, region and AWS profile`,
		},
		{
			name: "jump host offline", action: "ssm:StartSession", code: "TargetNotConnected", message: "i-scenario is not connected.",
			want: "database session: open tunnel: SSM StartSession for i-scenario failed: i-scenario is not connected. Check that the jump host is online in SSM (aws ssm describe-instance-information)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScenario(t)
			s.ssm = tc.action == "ssm:StartSession"
			p := s.secretsManagerProfile()
			if tc.setup != nil {
				tc.setup(p)
			}
			if tc.code == "" {
				s.aws.deny(tc.action)
			} else {
				s.aws.fail(tc.action, tc.code, tc.message)
			}
			assert.EqualError(t, s.fail(p), tc.want)
		})
	}
}
