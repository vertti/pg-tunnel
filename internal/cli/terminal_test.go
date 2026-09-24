package cli //nolint:testpackage // Test the private terminal reader's descriptor-level cancellation without requiring a controlling terminal in CI.

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/ptytest"
)

func TestTerminalInputReadsAndCancelsWhileWaiting(t *testing.T) {
	t.Parallel()
	descriptors := make([]int, 2)
	require.NoError(t, unix.Pipe(descriptors))
	t.Cleanup(func() {
		assert.NoError(t, unix.Close(descriptors[0]))
		assert.NoError(t, unix.Close(descriptors[1]))
	})
	require.NoError(t, unix.SetNonblock(descriptors[0], true))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := terminalInput{fd: descriptors[0], done: ctx.Done()}
	_, err := unix.Write(descriptors[1], []byte("answer\n"))
	require.NoError(t, err)
	buffer := make([]byte, 32)
	count, err := input.Read(buffer)
	require.NoError(t, err)
	assert.Equal(t, "answer\n", string(buffer[:count]))
	finished := make(chan error, 1)
	go func() { _, readErr := input.Read(buffer); finished <- readErr }()
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()
	select {
	case err = <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("terminal read did not stop after cancellation")
	}
}

// init reads /dev/tty, which macOS poll reports as always ready. A helper with a
// pseudo-terminal as its controlling terminal measures an idle read.
func TestTerminalInputWaitsWithoutSpinning(t *testing.T) {
	t.Parallel()
	primary, replicaName, err := ptytest.Open()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, primary.Close()) })
	replica, err := os.OpenFile(replicaName, os.O_RDWR|unix.O_NOCTTY, 0) //nolint:gosec // The path names the pseudo-terminal opened above.
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	helper := exec.CommandContext(t.Context(), executable, "-test.run=^TestIdleTerminalHelper$") //nolint:gosec // Runs this test binary with a controlling terminal.
	helper.Env = append(os.Environ(), "PG_TUNNEL_IDLE_TERMINAL_HELPER=1")
	helper.Stdin, helper.Stdout, helper.Stderr = replica, replica, replica
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	require.NoError(t, helper.Start())
	require.NoError(t, replica.Close())
	go io.Copy(io.Discard, primary) //nolint:errcheck // Drain helper output so it never blocks.
	require.NoError(t, helper.Wait(), "an idle prompt must not busy-wait")
}

func TestIdleTerminalHelper(t *testing.T) {
	if os.Getenv("PG_TUNNEL_IDLE_TERMINAL_HELPER") == "" {
		t.Skip("subprocess fixture")
	}
	terminal, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer unix.Close(terminal) //nolint:errcheck // Test fixture cleanup.
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	before := cpuTime(t)
	_, err = terminalInput{fd: terminal, done: ctx.Done()}.Read(make([]byte, 32))
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, cpuTime(t)-before, 150*time.Millisecond)
}

func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var usage unix.Rusage
	require.NoError(t, unix.Getrusage(unix.RUSAGE_SELF, &usage))
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
