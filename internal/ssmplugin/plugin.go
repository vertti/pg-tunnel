// Package ssmplugin runs AWS's official port forwarding code in a dedicated child.
// AWS installs signal handlers and calls os.Exit; never call Run in the supervisor.
package ssmplugin

import (
	"errors"
	"io"
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
	session.ValidateInputAndStartSession(append([]string{"pg-tunnel"}, args...), output)
	return nil
}
