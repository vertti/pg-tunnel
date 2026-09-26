package awsdb_test

import (
	"context"
	"encoding/binary"
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
	ssmlog "github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/message"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twinj/uuid"

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
	port chan string
	url  string
	fakeSSM
}

// StartSession directs the real embedded AWS code to a local WebSocket fixture.
func (f *localSSM) StartSession(_ context.Context, input *ssm.StartSessionInput, _ ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	if f.port != nil {
		f.port <- input.Parameters["localPortNumber"][0]
	}
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
	// Release smoke tests supply the packaged executable; otherwise use this test binary.
	transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example", Executable: os.Getenv("PG_TUNNEL_TEST_EXECUTABLE")}
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
	var handshake struct{ TokenValue string }
	if err := conn.ReadJSON(&handshake); err != nil {
		return fmt.Errorf("read AWS data-channel handshake: %w", err)
	}
	if handshake.TokenValue != "sensitive-token" {
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
[ "$2" = 'eu-central-1' ] || exit 9
[ "$4" = 'i-example' ] || exit 10
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

func TestEmbeddedShutdownSendsTerminationFlag(t *testing.T) {
	// Keep AWS's socket path below macOS's Unix socket length limit.
	directory, err := os.MkdirTemp("/tmp", "ssm-") //nolint:usetesting // t.TempDir includes the test name and exceeds the macOS socket path limit.
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	t.Setenv("TMPDIR", directory)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	port := make(chan string, 1)
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result <- forwardingHandshake(w, r, <-port)
	}))
	defer server.Close()
	api := &localSSM{url: "ws" + strings.TrimPrefix(server.URL, "http") + "/?X-Amz-Signature=fixture", port: port}
	// Release smoke tests supply the packaged executable; otherwise use this test binary.
	transport := awsdb.SSM{API: api, Region: "eu-central-1", Target: "i-example", Executable: os.Getenv("PG_TUNNEL_TEST_EXECUTABLE")}
	tunnel, err := transport.Open(ctx, session.Target{Host: "db.example", Port: 5432})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tunnel.Close(t.Context())) })
	closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer closeCancel()
	require.NoError(t, tunnel.Close(closeCtx))
	select {
	case shutdownErr := <-result:
		require.NoError(t, shutdownErr, "the AWS handler must send its termination flag before exiting")
	case <-ctx.Done():
		t.Fatal("timed out waiting for graceful SSM shutdown")
	}
	assert.Equal(t, "session-example", api.terminated)
	assert.Greater(t, api.terminateTime, 4*time.Second, "remote termination has its own time budget")
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	assert.Empty(t, entries, "AWS must remove its multiplexing socket")
}

// Use upstream serialization to exercise the real multiplexed port handler.
func forwardingHandshake(w http.ResponseWriter, r *http.Request, port string) error {
	var upgrader websocket.Upgrader
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("upgrade forwarding fixture: %w", err)
	}
	defer conn.Close() //nolint:errcheck // Fixture teardown after child shutdown.
	if err := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return fmt.Errorf("set forwarding deadline: %w", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		return fmt.Errorf("read initial token: %w", err)
	}
	payload := fmt.Sprintf(`{"AgentVersion":"3.3.0.0","RequestedClientActions":[{"ActionType":"SessionType","ActionParameters":{"SessionType":"Port","Properties":{"type":"LocalPortForwarding","localPortNumber":%q}}}]}`, port)
	logger := ssmlog.Logger(false, "pg-tunnel-test")
	for index, body := range []string{payload, `{}`} {
		payloadType := message.HandshakeRequestPayloadType
		if index == 1 {
			payloadType = message.HandshakeCompletePayloadType
		}
		packet := message.ClientMessage{MessageType: message.OutputStreamMessage, SchemaVersion: 1, CreatedDate: 1, SequenceNumber: int64(index), MessageId: uuid.NewV4(), PayloadType: uint32(payloadType), Payload: []byte(body)}
		encoded, err := packet.SerializeClientMessage(logger)
		if err != nil {
			return fmt.Errorf("serialize forwarding handshake: %w", err)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, encoded); err != nil {
			return fmt.Errorf("send forwarding handshake: %w", err)
		}
	}
	return receiveTermination(conn, logger)
}

func receiveTermination(conn *websocket.Conn, logger ssmlog.T) error {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read SSM termination flag: %w", err)
		}
		var packet message.ClientMessage
		if err := packet.DeserializeClientMessage(logger, data); err != nil {
			return fmt.Errorf("decode SSM packet: %w", err)
		}
		if packet.PayloadType == uint32(message.Flag) && len(packet.Payload) == 4 && binary.BigEndian.Uint32(packet.Payload) == uint32(message.TerminateSession) {
			return nil
		}
	}
}
