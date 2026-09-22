package awsdb_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/session"
	"github.com/vertti/pg-tunnel/internal/ssmplugin"
)

func TestMain(m *testing.M) {
	// Re-execution follows the same dispatch as cmd/pg-tunnel, before testing flags.
	if len(os.Args) > 1 && os.Args[1] == ssmplugin.Command {
		if err := ssmplugin.Run(os.Args[2:], os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type localSSM struct {
	fakeSSM
	url string
}

// StartSession directs the real embedded AWS code to a local WebSocket fixture.
func (f *localSSM) StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	return &ssm.StartSessionOutput{SessionId: aws.String("session-example"), TokenValue: aws.String("sensitive-token"), StreamUrl: aws.String(f.url)}, nil
}

func TestEmbeddedStartupCancellationCleansUp(t *testing.T) {
	// Empty PATH proves there is no dependency on an installed plugin or AWS CLI.
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	received := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- receiveSessionToken(w, r, cancel)
	}))
	defer server.Close()
	api := &localSSM{url: "ws" + strings.TrimPrefix(server.URL, "http") + "/?X-Amz-Signature=fixture"}
	transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example"}
	_, err := transport.Open(ctx, session.Target{Host: "db.example", Port: 5432})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "session-example", api.terminated)
	select {
	case err := <-received:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("embedded child did not finish the handshake and disconnect")
	}
}

func receiveSessionToken(w http.ResponseWriter, r *http.Request, cancel context.CancelFunc) error {
	var upgrader websocket.Upgrader
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("upgrade fixture WebSocket: %w", err)
	}
	defer conn.Close() //nolint:errcheck // Fixture teardown; the child can close first.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set fixture deadline: %w", err)
	}
	var message struct{ TokenValue string }
	if err := conn.ReadJSON(&message); err != nil {
		return fmt.Errorf("read AWS data-channel handshake: %w", err)
	}
	if message.TokenValue != "sensitive-token" {
		return errors.New("AWS data-channel handshake omitted the session token")
	}
	cancel()
	if _, _, err := conn.ReadMessage(); err == nil {
		return errors.New("embedded child sent data instead of disconnecting")
	} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		return fmt.Errorf("embedded child did not disconnect: %w", err)
	}
	return nil
}

func TestChildArgumentsKeepTokenInEnvironment(t *testing.T) {
	t.Parallel()
	// A tiny executable records the child contract and echoes the fake token to
	// exercise output redaction. The actual AWS handshake is tested above.
	executable := t.TempDir() + "/child"
	script := `#!/bin/sh
[ "$1" = '__ssm' ] || exit 8
[ "$2" = 'AWS_SSM_START_SESSION_RESPONSE' ] || exit 9
[ "$4" = 'StartSession' ] || exit 10
case "$*" in *sensitive-token*) exit 11;; esac
printf '%s' "$AWS_SSM_START_SESSION_RESPONSE"
exit 7
`
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700)) //nolint:gosec // Only the owner may execute the test fixture.
	api := &fakeSSM{}
	transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example", Executable: executable}
	_, err := transport.Open(t.Context(), session.Target{Host: "db.example", Port: 5432})
	require.ErrorContains(t, err, "exit status 7")
	assert.NotContains(t, err.Error(), "sensitive-token")
	assert.Contains(t, err.Error(), "[redacted]")
	assert.Equal(t, "session-example", api.terminated)
}
