package cli

import (
	"context"
	"encoding/json"
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
		runner.Transport = directTransport{port: s.postgres.Port}
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
	require.Eventually(s.t, func() bool { return strings.Contains(s.output(), substring) }, 20*time.Second, 50*time.Millisecond, "waiting for %q in:\n%s", substring, s.output())
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

// fakeAWS answers the AWS JSON-protocol calls pg-tunnel makes.
type fakeAWS struct {
	t        *testing.T
	mu       sync.Mutex
	password string
	version  int
}

func (f *fakeAWS) rotateSecret(password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.password = password
	f.version++
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	operation := r.Header.Get("X-Amz-Target")
	switch operation {
	case "secretsmanager.GetSecretValue":
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
