// Package ssmplugin runs AWS's official port forwarding code in a dedicated child.
// AWS installs signal handlers and calls os.Exit; never call Run in the supervisor.
package ssmplugin

import (
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/portsession" // Register AWS's port forwarding handler only.
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/sessionutil"
)

// Command selects the internal child mode before the normal CLI and signal setup.
const Command = "__ssm"

// ResponseEnv carries session details without exposing the token in process arguments.
const ResponseEnv = "AWS_SSM_START_SESSION_RESPONSE"

// Run invokes the pinned upstream entry point. It may exit the calling process.
func Run(args []string, output io.Writer) error {
	if len(args) != 6 || args[0] != ResponseEnv || args[2] != "StartSession" {
		return errors.New("invalid internal SSM invocation")
	}
	// The supervisor sends SIGTERM; upstream otherwise bypasses its graceful handler.
	sessionutil.ControlSignals = append(sessionutil.ControlSignals, syscall.SIGTERM)
	go StopWhenSupervisorExits(os.Stdin)
	session.ValidateInputAndStartSession(append([]string{"pg-tunnel"}, args...), output)
	return nil
}

// StopWhenSupervisorExits ends the session gracefully once the supervisor's end of
// stdin closes, which also happens when the supervisor is killed.
func StopWhenSupervisorExits(stdin io.Reader) {
	io.Copy(io.Discard, stdin)                 //nolint:errcheck,gosec // Any read failure means the supervisor is gone.
	syscall.Kill(os.Getpid(), syscall.SIGTERM) //nolint:errcheck,gosec // Signalling the current process cannot fail.
}
