package process_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestCanceledContextDoesNotStartProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := process.Start(ctx, []string{"sh", "-c", "exit 0"}, nil, nil, io.Discard, io.Discard)
	require.ErrorIs(t, err, context.Canceled)
}
