package cli //nolint:testpackage // Test the private terminal reader's descriptor-level cancellation without requiring a controlling terminal in CI.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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
