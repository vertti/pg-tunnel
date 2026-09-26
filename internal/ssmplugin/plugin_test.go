package ssmplugin_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/ssmplugin"
)

func TestRejectsUnsupportedChildInvocations(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"--version"},
		{"region", "", "i-test"},
		{"AWS_SSM_START_SESSION_RESPONSE", "region", "StartSession", "", "{}", "endpoint"},
	} {
		require.ErrorContains(t, ssmplugin.Run(args, io.Discard), "invalid internal SSM invocation")
	}
}

func TestChildStopsWhenSupervisorLifelineCloses(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	require.NoError(t, err)
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestLifelineChild$") //nolint:gosec // Runs this test binary as the embedded child.
	child.Env = append(os.Environ(), "PG_TUNNEL_LIFELINE_CHILD=1")
	lifeline, err := child.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	require.NoError(t, lifeline.Close())
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	select {
	case err = <-exited:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		status, ok := exit.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		assert.Equal(t, syscall.SIGTERM, status.Signal())
	case <-time.After(10 * time.Second):
		require.NoError(t, child.Process.Kill())
		t.Fatal("child kept running after its supervisor's lifeline closed")
	}
}

func TestLifelineChild(t *testing.T) {
	if os.Getenv("PG_TUNNEL_LIFELINE_CHILD") == "" {
		t.Skip("subprocess fixture")
	}
	go ssmplugin.StopWhenSupervisorExits(os.Stdin)
	time.Sleep(time.Minute)
	t.Error(errors.New("SIGTERM was not delivered"))
}
