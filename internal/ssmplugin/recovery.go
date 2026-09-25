package ssmplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/aws/session-manager-plugin/src/communicator"
	"github.com/aws/session-manager-plugin/src/datachannel"
	"github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/smithy-go"
)

// RecoveryStarted and RecoveryResumed are the only child progress messages forwarded live.
const (
	RecoveryStarted = "SSM connection interrupted; resuming the existing session for up to 1m. Client remains running; database operations may fail."
	RecoveryResumed = "SSM data channel resumed; client and local port retained. Retry failed database operations only when safe; SQL is not replayed."
)

// Install the callback before AWS opens the socket, without changing its protocol.
type recoveryChannel struct {
	*datachannel.DataChannel
	resume func() error
	output io.Writer
}

// Initialize installs recovery before the upstream socket starts receiving.
func (c *recoveryChannel) Initialize(logger log.T, clientID, sessionID, targetID string, upgrade bool) {
	c.DataChannel.Initialize(logger, clientID, sessionID, targetID, upgrade)
	socket := &recoverySocket{IWebSocketChannel: c.GetWsChannel()}
	socket.IWebSocketChannel.SetOnError(func(error) {
		socket.mu.Lock()
		defer socket.mu.Unlock()
		fmt.Fprintln(c.output, RecoveryStarted) //nolint:errcheck // Diagnostics are best effort; the supervisor owns the output pipe.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := resumeWithin(ctx, c.resume); err != nil {
			fmt.Fprintln(c.output, err) //nolint:errcheck // Exit and supervisor cleanup must still happen if output fails.
			stopAfterRecovery()
		}
		fmt.Fprintln(c.output, RecoveryResumed) //nolint:errcheck // Diagnostics are best effort; the supervisor owns the output pipe.
	})
	c.SetWsChannel(socket)
}

type recoverySocket struct {
	communicator.IWebSocketChannel
	mu sync.Mutex
}

// SetOnError keeps the preinstalled callback when AWS installs its finite retry loop.
func (*recoverySocket) SetOnError(func(error)) {}

const recoveryExpired = "SSM recovery exceeded 1m; stopping the tunnel and child command. Check network access and AWS login before restarting"

func resumeWithin(ctx context.Context, resume func() error) error {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return errors.New(recoveryExpired)
		}
		result := make(chan error, 1)
		go func() { result <- resume() }()
		select {
		case <-ctx.Done():
			return errors.New(recoveryExpired)
		case err := <-result:
			if err == nil {
				return nil
			}
			if reason := terminalResumeError(err); reason != "" {
				return fmt.Errorf("SSM resume rejected: %s; stopping the tunnel and child command. Check AWS login and session access before restarting", reason)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New(recoveryExpired)
		case <-timer.C:
		}
		delay = min(2*delay, 5*time.Second)
	}
}

// Only known error codes become diagnostics; upstream error text can contain secrets.
func terminalResumeError(err error) string {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return ""
	}
	switch api.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation":
		return "AWS denied ssm:ResumeSession"
	case "ExpiredToken", "ExpiredTokenException":
		return "AWS credentials expired"
	case "InvalidClientTokenId", "UnrecognizedClientException":
		return "AWS credentials were rejected"
	case "InvalidSessionId", "InvalidSessionIdException", "DoesNotExistException":
		return "the SSM session no longer exists"
	default:
		return ""
	}
}

// AWS calls do not accept our context. Allow its signal handler to remove the
// multiplexing socket, then force exit if it hangs. The supervisor owns remote cleanup.
func stopAfterRecovery() {
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err == nil {
		time.Sleep(3 * time.Second)
	}
	os.Exit(1)
}
