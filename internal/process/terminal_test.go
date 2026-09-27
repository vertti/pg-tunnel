package process_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/ptytest"
	"github.com/vertti/pg-tunnel/internal/testutil"
)

// A job-control shell runs the supervisor; its child stops while holding the
// terminal, as psql does on Ctrl-Z. The shell must regain control, and fg must
// resume the child.
func TestStoppedChildReturnsTerminalToShell(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash provides the job-control shell")
	}
	supervisor := testutil.SelfCommand(t, "TestJobControlSupervisor", "PG_TUNNEL_JOB_CONTROL_SUPERVISOR=1")
	script := `"$0" "$@"; echo "stopped=$?"; fg >/dev/null; echo "final=$?"`
	shell := exec.CommandContext(t.Context(), "bash", append([]string{"--norc", "--noprofile", "-i", "-c", script}, supervisor.Args...)...) //nolint:gosec // Runs this test binary under a job-control shell.
	shell.Env = supervisor.Env
	primary, err := ptytest.Start(shell)
	require.NoError(t, err)
	defer primary.Close() //nolint:errcheck // Test cleanup of the terminal primary.

	var output bytes.Buffer
	var mu sync.Mutex
	go func() {
		chunk := make([]byte, 1024)
		for {
			count, readErr := primary.Read(chunk)
			mu.Lock()
			output.Write(chunk[:count])
			mu.Unlock()
			if readErr != nil {
				return
			}
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- shell.Wait() }()
	select {
	case err = <-exited:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		require.NoError(t, syscall.Kill(-shell.Process.Pid, syscall.SIGKILL))
		<-exited
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the shell never regained the terminal; output: %q", output.String())
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(output.String(), "final=")
	}, 5*time.Second, 10*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	text := output.String()
	stoppedAt, resumedAt := strings.Index(text, "stopped="), strings.Index(text, "resumed")
	assert.True(t, stoppedAt >= 0 && resumedAt > stoppedAt, "the supervisor stops before the child resumes: %q", text)
	assert.NotContains(t, text, "stopped=0")
	assert.Contains(t, text, "final=0")
}

func TestJobControlSupervisor(t *testing.T) {
	if os.Getenv("PG_TUNNEL_JOB_CONTROL_SUPERVISOR") == "" {
		t.Skip("subprocess fixture")
	}
	require.NoError(t, process.Run(t.Context(), []string{"sh", "-c", "kill -STOP $$; echo resumed"}, os.Environ()))
}
