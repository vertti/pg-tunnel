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
	handle := &tunnel{api: s.API, sessionID: aws.ToString(output.SessionId), token: aws.ToString(output.TokenValue), port: port, logs: &logTail{}}
	defer func() {
		if result != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cleanupCancel()
			result = errors.Join(result, handle.Close(cleanupCtx))
		}
	}()
	if err := s.launch(startupCtx, path, input, output, handle); err != nil {
		return nil, err
	}
	return handle, nil
}

func (s *SSM) launch(ctx context.Context, path string, input *ssm.StartSessionInput, output *ssm.StartSessionOutput, handle *tunnel) error {
	if handle.sessionID == "" || handle.token == "" || aws.ToString(output.StreamUrl) == "" {
		return errors.New("SSM returned incomplete session details")
	}
	args, env, err := s.pluginCommand(ctx, path, input, output)
	if err != nil {
		return err
	}
	handle.process, err = process.Start(ctx, args, env, nil, handle.logs, handle.logs)
	if err != nil {
		return fmt.Errorf("launch embedded SSM child: %w", err)
	}
	return handle.waitReady(ctx)
}

func (s *SSM) pluginCommand(ctx context.Context, path string, input *ssm.StartSessionInput, output *ssm.StartSessionOutput) (args, env []string, result error) {
	response, err := json.Marshal(output)
	if err != nil {
		return nil, nil, fmt.Errorf("encode SSM response: %w", err)
	}
	request, err := json.Marshal(input)
	if err != nil {
		return nil, nil, fmt.Errorf("encode SSM request: %w", err)
	}
	endpoint, err := ssm.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, ssm.EndpointParameters{Region: aws.String(s.Region)})
	if err != nil {
		return nil, nil, fmt.Errorf("resolve SSM endpoint: %w", err)
	}
	const responseEnv = ssmplugin.ResponseEnv
	args = []string{path, ssmplugin.Command, responseEnv, s.Region, "StartSession", s.Profile, string(request), endpoint.URI.String()}
	env = append(os.Environ(), responseEnv+"="+string(response))
	return args, env, nil
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
	return fmt.Errorf("embedded SSM child exited: %s: %w", strings.ReplaceAll(t.logs.String(), t.token, "[redacted]"), err)
}

// Close terminates both the local plugin and remote session.
func (t *tunnel) Close(ctx context.Context) error {
	t.once.Do(func() {
		if t.process != nil {
			t.closeErr = t.process.Stop(ctx)
		}
		if t.sessionID == "" {
			return
		}
		t.closeErr = errors.Join(t.closeErr, t.terminateRemote(ctx))
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
	text string
	mu   sync.Mutex
}

func (l *logTail) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text += string(data)
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
