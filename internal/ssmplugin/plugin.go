// Package ssmplugin runs AWS's official port forwarding code in a dedicated child.
// AWS installs signal handlers and calls os.Exit; never call Run in the supervisor.
package ssmplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/session-manager-plugin/src/datachannel"
	"github.com/aws/session-manager-plugin/src/log"
	"github.com/aws/session-manager-plugin/src/sdkutil"
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/portsession" // Register AWS's port forwarding handler only.
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/sessionutil"
	"github.com/twinj/uuid"
)

// Command selects the internal child mode before the normal CLI and signal setup.
const Command = "__ssm"

// ResponseEnv carries session details without exposing the token in process arguments.
const ResponseEnv = "AWS_SSM_START_SESSION_RESPONSE"

// Run invokes the pinned upstream session with bounded recovery. It may exit the calling process.
func Run(args []string, output io.Writer) error {
	if len(args) != 4 {
		return errors.New("invalid internal SSM invocation")
	}
	s, err := newSession(args)
	if err != nil {
		return err
	}
	// The supervisor sends SIGTERM; upstream otherwise bypasses its graceful handler.
	sessionutil.ControlSignals = append(sessionutil.ControlSignals, syscall.SIGTERM)
	go StopWhenSupervisorExits(os.Stdin)
	logger := log.Logger(true, "session-manager-plugin")
	s.DataChannel = &recoveryChannel{DataChannel: &datachannel.DataChannel{}, resume: func() error { return s.ResumeSessionHandler(logger) }, output: output}
	if err := s.Execute(logger); err != nil {
		return fmt.Errorf("start embedded SSM session: %w", err)
	}
	return nil
}

// StopWhenSupervisorExits ends the session gracefully once the supervisor's end of
// stdin closes, which also happens when the supervisor is killed.
func StopWhenSupervisorExits(stdin io.Reader) {
	io.Copy(io.Discard, stdin)                 //nolint:errcheck,gosec // Any read failure means the supervisor is gone.
	syscall.Kill(os.Getpid(), syscall.SIGTERM) //nolint:errcheck,gosec // Signalling the current process cannot fail.
}

// Keep token handling private while using AWS's session, handshake and forwarding code.
// args are region, AWS profile, target, and SSM endpoint.
func newSession(args []string) (*session.Session, error) {
	response := os.Getenv(ResponseEnv)
	if err := os.Unsetenv(ResponseEnv); err != nil {
		return nil, fmt.Errorf("remove SSM response environment: %w", err)
	}
	var start ssm.StartSessionOutput
	if json.Unmarshal([]byte(response), &start) != nil {
		return nil, errors.New("invalid internal SSM response")
	}
	s := &session.Session{SessionId: aws.ToString(start.SessionId), StreamUrl: aws.ToString(start.StreamUrl), TokenValue: aws.ToString(start.TokenValue), TargetId: args[2], Region: args[0], Endpoint: args[3]}
	if s.SessionId == "" || s.StreamUrl == "" || s.TokenValue == "" || s.TargetId == "" {
		return nil, errors.New("incomplete internal SSM session")
	}
	sdkutil.SetRegionAndProfile(args[0], args[1])
	uuid.SwitchFormat(uuid.CleanHyphen)
	s.ClientId = uuid.NewV4().String()
	return s, nil
}
