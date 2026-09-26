// Package ssmplugin runs AWS's official port forwarding code in a dedicated child.
// AWS installs signal handlers and calls os.Exit; never call Run in the supervisor.
package ssmplugin

import (
	"bufio"
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
	"github.com/cihub/seelog"
	"github.com/twinj/uuid"
)

// Command selects the internal child mode before the normal CLI and signal setup.
const Command = "__ssm"

// Run invokes the pinned upstream session with bounded recovery. It may exit the calling process.
// The first stdin line is the StartSession response, which keeps the token out of
// process arguments and environment.
func Run(args []string, output io.Writer) error {
	if len(args) != 4 {
		return errors.New("invalid internal SSM invocation")
	}
	stdin := bufio.NewReader(os.Stdin)
	response, err := stdin.ReadBytes('\n')
	if err != nil {
		return errors.New("read internal SSM response")
	}
	s, err := newSession(args, response)
	if err != nil {
		return err
	}
	// The supervisor sends SIGTERM; upstream otherwise bypasses its graceful handler.
	sessionutil.ControlSignals = append(sessionutil.ControlSignals, syscall.SIGTERM)
	go StopWhenSupervisorExits(stdin)
	logger := quietLogger{seelog.Disabled}
	s.DataChannel = &recoveryChannel{DataChannel: &datachannel.DataChannel{}, resume: func() error { return s.ResumeSessionHandler(logger) }, output: output}
	if err := s.Execute(logger); err != nil {
		return fmt.Errorf("start embedded SSM session: %w", err)
	}
	return nil
}

// quietLogger keeps upstream from reading and watching a seelog.xml it does not own.
type quietLogger struct{ seelog.LoggerInterface }

// WithContext returns the same disabled logger.
func (l quietLogger) WithContext(...string) log.T { return l }

// StopWhenSupervisorExits ends the session gracefully once the supervisor's end of
// stdin closes, which also happens when the supervisor is killed.
func StopWhenSupervisorExits(stdin io.Reader) {
	io.Copy(io.Discard, stdin)                 //nolint:errcheck,gosec // Any read failure means the supervisor is gone.
	syscall.Kill(os.Getpid(), syscall.SIGTERM) //nolint:errcheck,gosec // Signalling the current process cannot fail.
}

// Keep token handling private while using AWS's session, handshake and forwarding code.
// args are region, AWS profile, target, and SSM endpoint.
func newSession(args []string, response []byte) (*session.Session, error) {
	var start ssm.StartSessionOutput
	if json.Unmarshal(response, &start) != nil {
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
