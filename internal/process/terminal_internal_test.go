package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoppedTracksJobControlState(t *testing.T) {
	t.Parallel()
	group, err := Start(t.Context(), []string{"sleep", "60"}, os.Environ(), nil, io.Discard, io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, group.Stop(ctx))
	})
	pid := group.cmd.Process.Pid
	assert.False(t, stopped(pid))
	require.NoError(t, syscall.Kill(pid, syscall.SIGSTOP))
	require.Eventually(t, func() bool { return stopped(pid) }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, syscall.Kill(pid, syscall.SIGCONT))
	require.Eventually(t, func() bool { return !stopped(pid) }, 5*time.Second, 10*time.Millisecond)
	assert.False(t, stopped(-1))
}

func TestHandoverRequiresForegroundTerminal(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, errors.Join(reader.Close(), writer.Close())) })
	for _, stdin := range []io.Reader{nil, &bytes.Buffer{}, reader} {
		cmd := exec.CommandContext(t.Context(), "true")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		assert.Nil(t, handover(cmd, stdin))
		assert.False(t, cmd.SysProcAttr.Foreground)
	}
}
