package ssmplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/session-manager-plugin/src/communicator"
	"github.com/aws/session-manager-plugin/src/datachannel"
	"github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/sdkutil"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResumeSurvivesThirtySecondOutage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		attempts := 0
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		err := resumeWithin(ctx, func() error {
			attempts++
			if time.Since(start) < 30*time.Second {
				return errors.New("network unavailable")
			}
			return nil
		})
		require.NoError(t, err)
		assert.Greater(t, attempts, 6, "must outlast AWS's original retry loop")
		assert.Less(t, time.Since(start), time.Minute)
	})
}

func TestResumeDeadlineIncludesBlockedAWSCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		release := make(chan struct{})
		defer close(release)
		start := time.Now()
		err := resumeWithin(ctx, func() error { <-release; return nil })
		require.ErrorContains(t, err, "SSM recovery exceeded 1m")
		assert.Equal(t, time.Minute, time.Since(start))
	})
}

func TestResumeDeadlineIncludesRepeatedFailures(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		start := time.Now()
		err := resumeWithin(ctx, func() error { return errors.New("secret-looking upstream failure") })
		require.ErrorContains(t, err, "SSM recovery exceeded 1m")
		assert.NotContains(t, err.Error(), "secret-looking")
		assert.Equal(t, time.Minute, time.Since(start))
	})
}

func TestResumeRejectsExpiredOrDeniedSessionsWithoutRetry(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"AccessDeniedException", "ExpiredTokenException", "InvalidSessionId", "DoesNotExistException"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			err := resumeWithin(t.Context(), func() error {
				attempts++
				return fmt.Errorf("resume: %w", &smithy.GenericAPIError{Code: code, Message: "private token"})
			})
			require.ErrorContains(t, err, "SSM resume rejected")
			assert.NotContains(t, err.Error(), "private token")
			assert.Equal(t, 1, attempts)
		})
	}
}

func TestNewSessionRetainsAWSInputs(t *testing.T) {
	s, err := newSession([]string{"eu-central-1", "", "i-test", "https://ssm.example"}, []byte(`{"SessionId":"session-test","TokenValue":"secret-token","StreamUrl":"wss://ssmmessages.example/channel"}`+"\n"))
	require.NoError(t, err)
	assert.Equal(t, "session-test", s.SessionId)
	assert.Equal(t, "secret-token", s.TokenValue)
	assert.Equal(t, "wss://ssmmessages.example/channel", s.StreamUrl)
	assert.Equal(t, "i-test", s.TargetId)
	assert.Equal(t, "https://ssm.example", s.Endpoint)
	assert.Equal(t, "eu-central-1", sdkutil.GetRegion())
	assert.Len(t, s.ClientId, 36)
}

func TestInvalidSessionResponseIsRedacted(t *testing.T) {
	for _, response := range []string{`secret-token`, `{"TokenValue":"secret-token"}`} {
		_, err := newSession([]string{"eu-central-1", "", "i-test", "https://ssm.example"}, []byte(response))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret-token")
	}
}

func TestRecoveryCallbackReplacesUpstreamRetryHandler(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	attempts := 0
	channel := recoveryChannel{DataChannel: &datachannel.DataChannel{}, output: &output, resume: func() error { attempts++; return nil }}
	channel.Initialize(log.Logger(false, "test"), "client", "session", "target", false)
	socket, ok := channel.GetWsChannel().(*recoverySocket)
	require.True(t, ok)
	socket.SetOnError(func(error) { t.Error("upstream retry handler must not replace bounded recovery") })
	native, ok := socket.IWebSocketChannel.(*communicator.WebSocketChannel)
	require.True(t, ok)
	native.OnError(errors.New("private upstream error"))
	assert.Equal(t, 1, attempts)
	assert.Equal(t, RecoveryStarted+"\n"+RecoveryResumed+"\n", output.String())
}

func TestRejectedResumeExitsChild(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestRecoveryChild$") //nolint:gosec // Execute this test binary as the isolated plugin process.
	child.Env = append(os.Environ(), "PG_TUNNEL_RECOVERY_CHILD=1")
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	status, ok := exit.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.Equal(t, syscall.SIGTERM, status.Signal())
	assert.Contains(t, string(output), "SSM resume rejected")
	assert.NotContains(t, string(output), "private token")
}

func TestRecoveryChild(t *testing.T) {
	if os.Getenv("PG_TUNNEL_RECOVERY_CHILD") == "" {
		t.Skip("subprocess fixture")
	}
	channel := recoveryChannel{DataChannel: &datachannel.DataChannel{}, output: os.Stdout, resume: func() error { return &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "private token"} }}
	channel.Initialize(log.Logger(false, "test"), "client", "session", "target", false)
	socket, ok := channel.GetWsChannel().(*recoverySocket)
	require.True(t, ok)
	native, ok := socket.IWebSocketChannel.(*communicator.WebSocketChannel)
	require.True(t, ok)
	native.OnError(errors.New("disconnected"))
	t.Fatal("rejected resume did not exit")
}

func TestAWSResumeRejectsTerminalErrorsWithoutStartingAnother(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/absent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/absent")
	for _, tc := range []struct {
		name, code, body, source, expiry, reason string
		status, requests                         int
	}{
		{name: "missing session", code: "DoesNotExistException", reason: "the SSM session no longer exists", status: 400, requests: 1},
		{name: "expired token", code: "ExpiredTokenException", reason: "AWS credentials expired", status: 400, requests: 1},
		{name: "access denied", code: "AccessDeniedException", reason: "AWS denied ssm:ResumeSession", status: 400, requests: 1},
		{name: "plain forbidden", body: "Server authentication failed: <UnauthorizedRequest><message>Forbidden.</message></UnauthorizedRequest>\n", reason: "AWS rejected SSM authentication or access (HTTP 403)", status: 403, requests: 1},
		{name: "plain unauthorized", body: "private upstream detail", reason: "AWS rejected SSM authentication or access (HTTP 401)", status: 401, requests: 1},
		{name: "known expired environment", source: config.CredentialsSourceName, expiry: time.Now().Add(-time.Minute).Format(time.RFC3339), reason: "AWS credentials expired", requests: 0},
		{name: "valid environment expiry", source: config.CredentialsSourceName, expiry: time.Now().Add(time.Hour).Format(time.RFC3339), code: "DoesNotExistException", reason: "the SSM session no longer exists", status: 400, requests: 1},
		{name: "refreshable provider ignores environment expiry", source: "CredentialsEndpointProvider", expiry: time.Now().Add(-time.Minute).Format(time.RFC3339), code: "DoesNotExistException", reason: "the SSM session no longer exists", status: 400, requests: 1},
		{name: "plain server failure remains retryable", body: "private upstream detail", reason: "SSM recovery exceeded 1m", status: 503, requests: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_CREDENTIAL_EXPIRATION", tc.expiry)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "AmazonSSM.ResumeSession", r.Header.Get("X-Amz-Target"))
				var input struct {
					SessionID string `json:"SessionId"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, "terminated-test-session", input.SessionID)
				if tc.body != "" {
					w.Header().Set("Content-Type", "application/octet-stream")
					w.WriteHeader(tc.status)
					_, err := fmt.Fprint(w, tc.body)
					assert.NoError(t, err)
					return
				}
				w.Header().Set("Content-Type", "application/x-amz-json-1.1")
				w.WriteHeader(tc.status)
				_, err := fmt.Fprintf(w, `{"__type":%q,"Message":"private upstream detail"}`, tc.code)
				assert.NoError(t, err)
			}))
			defer server.Close()
			s, err := newSession([]string{"eu-central-1", "", "i-test", server.URL}, []byte(`{"SessionId":"terminated-test-session","TokenValue":"token","StreamUrl":"wss://ssmmessages.example/channel"}`))
			require.NoError(t, err)
			s.Credentials.Source = tc.source
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err = resumeWithin(ctx, func() error { return resumeSession(s, log.Logger(false, "test")) })
			require.ErrorContains(t, err, tc.reason)
			assert.NotContains(t, err.Error(), "private upstream detail")
			assert.NotContains(t, err.Error(), "<UnauthorizedRequest>")
			if tc.requests < 0 {
				assert.Positive(t, requests.Load())
			} else {
				assert.EqualValues(t, tc.requests, requests.Load())
			}
		})
	}
}
