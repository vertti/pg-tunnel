// Package process supervises Unix process groups without replacing the owner.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// Group owns a subprocess, all of its descendants, and its exit result.
type Group struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// Start starts a command in its own process group. The caller must call Stop.
func Start(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) (*Group, error) {
	if len(args) == 0 {
		return nil, errors.New("missing command after --")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}
	// Cancellation is owned by Stop so descendants are signalled and reaped too.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), args[0], args[1:]...) //nolint:gosec // Executing the explicitly requested client or trusted transport is the purpose of this utility.
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	term := handover(cmd, stdin)
	if term == nil {
		return start(cmd, func(int) {})
	}
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	group, err := start(cmd, term.restore)
	if err != nil {
		signal.Stop(sigchld)
		return nil, err
	}
	go func() {
		defer signal.Stop(sigchld)
		term.followStops(cmd.Process.Pid, sigchld, group.done)
	}()
	return group, nil
}

func start(cmd *exec.Cmd, restore func(child int)) (*Group, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Args[0], err)
	}
	group := &Group{cmd: cmd, done: make(chan struct{})}
	go func() { group.err = cmd.Wait(); restore(cmd.Process.Pid); close(group.done) }()
	return group, nil
}

// Done is closed when the subprocess exits.
func (g *Group) Done() <-chan struct{} { return g.done }

// Err returns the result only after Done is closed.
func (g *Group) Err() error {
	select {
	case <-g.done:
		return g.err
	default:
		return nil
	}
}

// Stop terminates the process group, escalating to SIGKILL after three seconds.
func (g *Group) Stop(ctx context.Context) error {
	return g.stop(ctx, syscall.SIGTERM)
}

func (g *Group) stop(ctx context.Context, sig syscall.Signal) error {
	if err := syscall.Kill(-g.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process group: %w", err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-g.done:
		// A child may have exited while its descendants are still running.
	case <-timer.C:
	case <-ctx.Done():
	}
	if err := syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group: %w", err)
	}
	<-g.done
	return nil
}

// Run supervises a user command and always terminates its remaining descendants.
func Run(ctx context.Context, args, env []string) (result error) {
	group, err := Start(ctx, args, env, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	stopSignal := syscall.SIGTERM
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		result = errors.Join(result, group.stop(cleanupCtx, stopSignal))
	}()
	select {
	case <-group.Done():
		return group.Err()
	case <-ctx.Done():
		var cause *signalError
		if errors.As(context.Cause(ctx), &cause) {
			stopSignal = cause.signal
		}
		return fmt.Errorf("command interrupted: %w", context.Cause(ctx))
	}
}

// ExitCode preserves the child's failure status after session cleanup.
func ExitCode(err error) int {
	var interrupted *signalError
	if errors.As(err, &interrupted) {
		return 128 + int(interrupted.signal)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		if status, ok := exit.Sys().(syscall.WaitStatus); ok {
			return 128 + int(status.Signal())
		}
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

// SignalContext preserves the received signal for forwarding and exit status.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case received := <-ch:
			if sig, ok := received.(syscall.Signal); ok {
				cancel(&signalError{signal: sig})
			}
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(ch); cancel(context.Canceled) }
}

type signalError struct{ signal syscall.Signal }

func (s *signalError) Error() string { return "received " + s.signal.String() }
func (*signalError) Unwrap() error   { return context.Canceled }
