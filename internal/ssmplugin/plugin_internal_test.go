package ssmplugin

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/testutil"
)

func TestChildStopsWhenSupervisorLifelineCloses(t *testing.T) {
	t.Parallel()
	child := testutil.SelfCommand(t, "TestLifelineChild", "PG_TUNNEL_LIFELINE_CHILD=1")
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
	go stopWhenSupervisorExits(os.Stdin)
	time.Sleep(time.Minute)
	t.Error("SIGTERM was not delivered")
}
