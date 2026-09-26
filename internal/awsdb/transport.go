package awsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/session"
	"github.com/vertti/pg-tunnel/internal/ssmplugin"
)

// SSMAPI is the session lifecycle used by the transport.
type SSMAPI interface {
	DescribeSessions(context.Context, *ssm.DescribeSessionsInput, ...func(*ssm.Options)) (*ssm.DescribeSessionsOutput, error)
	StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
	TerminateSession(context.Context, *ssm.TerminateSessionInput, ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error)
}

// SSM supervises the official plugin and owns the corresponding remote session.
type SSM struct {
	API        SSMAPI
	Report     func(string)
	Target     string
	Region     string
	Profile    string
	Executable string // Overrides os.Executable for process test harnesses.
	LocalPort  int
}

type tunnel struct {
	api       SSMAPI
	closeErr  error
	process   *process.Group
	lifeline  *os.File
	logs      *logTail
	sessionID string
	token     string
	port      int
	once      sync.Once
}

// Open starts a remote forwarding session and waits for the local listener.
func (s *SSM) Open(ctx context.Context, target session.Target) (_ session.Tunnel, result error) {
	path := s.Executable
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate pg-tunnel executable for SSM child: %w", err)
		}
	}
	port, err := availablePort(ctx, s.LocalPort)
	if err != nil {
		return nil, err
	}
	input := &ssm.StartSessionInput{Target: aws.String(s.Target), DocumentName: aws.String("AWS-StartPortForwardingSessionToRemoteHost"), Parameters: map[string][]string{
		"host": {target.Host}, "portNumber": {strconv.Itoa(target.Port)}, "localPortNumber": {strconv.Itoa(port)},
	}}
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := s.API.StartSession(startupCtx, input)
	if err != nil {
		return nil, fmt.Errorf("SSM StartSession for %s; check ssm:StartSession permission and the target's SSM connectivity: %w", s.Target, err)
	}
	handle := &tunnel{api: s.API, sessionID: aws.ToString(output.SessionId), token: aws.ToString(output.TokenValue), port: port, logs: &logTail{report: s.Report}}
	defer func() {
		if result != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cleanupCancel()
			result = errors.Join(result, handle.Close(cleanupCtx))
		}
	}()
	if err := s.launch(startupCtx, path, output, handle); err != nil {
		return nil, err
	}
	return handle, nil
}

func (s *SSM) launch(ctx context.Context, path string, output *ssm.StartSessionOutput, handle *tunnel) error {
	if handle.sessionID == "" || handle.token == "" || aws.ToString(output.StreamUrl) == "" {
		return errors.New("SSM returned incomplete session details")
	}
	response, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("encode SSM response: %w", err)
	}
	args, err := s.pluginArgs(ctx, path)
	if err != nil {
		return err
	}
	if err := startChild(ctx, args, response, handle); err != nil {
		return err
	}
	return handle.waitReady(ctx)
}

// maxBufferedResponse stays well below pipe capacity (64 KiB on Linux and macOS),
// so the response is written before the child starts without blocking.
const maxBufferedResponse = 8 << 10

// startChild sends the session response as the first stdin line; the child then
// stops when that pipe closes, even if the supervisor is killed.
func startChild(ctx context.Context, args []string, response []byte, handle *tunnel) error {
	if len(response) >= maxBufferedResponse {
		return fmt.Errorf("SSM session response is %d bytes; the limit is %d", len(response), maxBufferedResponse)
	}
	stdin, lifeline, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create SSM child lifeline: %w", err)
	}
	handle.lifeline = lifeline
	// Writing while the read end is still open here means a child that exits
	// early cannot turn its own failure into a broken-pipe error.
	if _, err = lifeline.Write(append(response, '\n')); err != nil {
		return errors.Join(fmt.Errorf("send SSM session to child: %w", err), stdin.Close())
	}
	handle.process, err = process.Start(ctx, args, os.Environ(), stdin, handle.logs, handle.logs)
	if closeErr := stdin.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("launch embedded SSM child: %w", err)
	}
	return nil
}

func (s *SSM) pluginArgs(ctx context.Context, path string) ([]string, error) {
	endpoint, err := ssm.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, ssm.EndpointParameters{Region: aws.String(s.Region)})
	if err != nil {
		return nil, fmt.Errorf("resolve SSM endpoint: %w", err)
	}
	return []string{path, ssmplugin.Command, s.Region, s.Profile, s.Target, endpoint.URI.String()}, nil
}

func availablePort(ctx context.Context, port int) (int, error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return 0, fmt.Errorf("reserve local port %d (another session may be using it): %w", port, err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.Join(errors.New("listener returned a non-TCP address"), listener.Close())
	}
	if err = listener.Close(); err != nil {
		return 0, fmt.Errorf("release reserved port: %w", err)
	}
	return address.Port, nil
}

// Port returns the loopback listening port.
func (t *tunnel) Port() int { return t.port }

// Done signals plugin exit.
func (t *tunnel) Done() <-chan struct{} { return t.process.Done() }

// Err reports plugin failure with the session token redacted.
func (t *tunnel) Err() error {
	err := t.process.Err()
	if err == nil {
		err = errors.New("unexpected successful exit")
	}
	// The child's exit status is not the user's command status, so it is not wrapped.
	return fmt.Errorf("embedded SSM child exited: %s: %s", strings.ReplaceAll(t.logs.String(), t.token, "[redacted]"), err.Error())
}

// Close terminates both the local plugin and remote session.
func (t *tunnel) Close(ctx context.Context) error {
	t.once.Do(func() {
		if t.process != nil {
			t.closeErr = t.process.Stop(ctx)
		}
		if t.lifeline != nil {
			t.closeErr = errors.Join(t.closeErr, t.lifeline.Close())
		}
		if t.sessionID == "" {
			return
		}
		// Stopping the plugin can use most of the caller's budget.
		remoteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		t.closeErr = errors.Join(t.closeErr, t.terminateRemote(remoteCtx))
	})
	return t.closeErr
}

// The graceful plugin can finish the session before our fallback API call.
// Confirm that outcome on API failure; never assume a rejected request succeeded.
func (t *tunnel) terminateRemote(ctx context.Context) error {
	_, err := t.api.TerminateSession(ctx, &ssm.TerminateSessionInput{SessionId: aws.String(t.sessionID)})
	if err == nil {
		return nil
	}
	terminationErr := fmt.Errorf("terminate SSM session %s: %w", t.sessionID, err)
	history, err := t.api.DescribeSessions(ctx, &ssm.DescribeSessionsInput{
		State:   types.SessionStateHistory,
		Filters: []types.SessionFilter{{Key: types.SessionFilterKeySessionId, Value: aws.String(t.sessionID)}},
	})
	if err != nil {
		return errors.Join(terminationErr, fmt.Errorf("confirm SSM session termination (requires ssm:DescribeSessions): %w", err))
	}
	for _, item := range history.Sessions {
		if aws.ToString(item.SessionId) == t.sessionID && item.Status == types.SessionStatusTerminated {
			return nil
		}
	}
	return terminationErr
}

func (t *tunnel) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for local tunnel listener; check the SSM agent, embedded child, and remote connectivity: %w", ctx.Err())
		case <-t.Done():
			return t.Err()
		case <-ticker.C:
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(t.port)))
			if err == nil {
				if err = conn.Close(); err != nil {
					return fmt.Errorf("close listener probe: %w", err)
				}
				return nil
			}
		}
	}
}

type logTail struct {
	report  func(string)
	text    string
	pending string
	mu      sync.Mutex
}

func (l *logTail) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text += string(data)
	l.pending += string(data)
	for {
		line, rest, found := strings.Cut(l.pending, "\n")
		if !found {
			break
		}
		l.pending = rest
		if l.report != nil && (line == ssmplugin.RecoveryStarted || line == ssmplugin.RecoveryResumed) {
			l.report(line)
		}
	}
	if len(l.pending) > 8192 {
		l.pending = ""
	}
	if len(l.text) > 8192 {
		l.text = l.text[len(l.text)-8192:]
	}
	return len(data), nil
}

func (l *logTail) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(l.text)
}
