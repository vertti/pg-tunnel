package process_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/process"
)

func TestChildExitStatusPreserved(t *testing.T) {
	t.Parallel()
	err := process.Run(t.Context(), []string{"sh", "-c", "exit 23"}, os.Environ())
	require.Error(t, err)
	assert.Equal(t, 23, process.ExitCode(err))
}

func TestCancelStopsAndJoinsProcessGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	group, err := process.Start(ctx, []string{"sh", "-c", "sleep 60 & wait"}, os.Environ(), nil, io.Discard, io.Discard)
	require.NoError(t, err)
	cleanupCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	require.NoError(t, group.Stop(cleanupCtx))
	select {
	case <-group.Done():
	default:
		t.Fatal("Stop returned before the process was reaped")
	}
}

func TestExitCodeReportsSignalsAndCancellation(t *testing.T) {
	t.Parallel()
	killed := process.Run(t.Context(), []string{"sh", "-c", "kill -KILL $$"}, os.Environ())
	assert.Equal(t, 128+int(syscall.SIGKILL), process.ExitCode(killed))
	assert.Equal(t, 130, process.ExitCode(fmt.Errorf("stopped: %w", context.Canceled)))
	assert.Equal(t, 1, process.ExitCode(errors.New("setup failed")))
}

func TestReceivedSignalIsForwardedAndPreserved(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "received")
			script := `trap 'echo received > "$0"; exit 0' ` + unix.SignalName(sig)[3:] + `; : > "$0.ready"; while :; do sleep 1; done`
			ctx, stop := process.SignalContext(t.Context())
			defer stop()
			result := make(chan error, 1)
			go func() { result <- process.Run(ctx, []string{"sh", "-c", script, record}, os.Environ()) }()
			require.Eventually(t, func() bool { _, err := os.Stat(record + ".ready"); return err == nil }, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, syscall.Kill(os.Getpid(), sig))
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
				require.ErrorContains(t, err, "received "+sig.String())
				assert.Equal(t, 128+int(sig), process.ExitCode(err))
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not stop the command", sig)
			}
			assert.FileExists(t, record, "the command must receive the same signal")
		})
	}
}

func TestErrIsEmptyUntilExit(t *testing.T) {
	t.Parallel()
	group, err := process.Start(t.Context(), []string{"sleep", "60"}, os.Environ(), nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NoError(t, group.Err())
	cleanupCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, group.Stop(cleanupCtx))
	require.Error(t, group.Err())
}

func TestMissingCommandIsRejected(t *testing.T) {
	t.Parallel()
	_, err := process.Start(t.Context(), nil, nil, nil, io.Discard, io.Discard)
	require.ErrorContains(t, err, "missing command")
	_, err = process.Start(t.Context(), []string{"/nonexistent/pg-tunnel-test"}, nil, nil, io.Discard, io.Discard)
	require.ErrorContains(t, err, "start /nonexistent/pg-tunnel-test")
}

func TestCanceledContextDoesNotStartProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := process.Start(ctx, []string{"sh", "-c", "exit 0"}, nil, nil, io.Discard, io.Discard)
	require.ErrorIs(t, err, context.Canceled)
}
