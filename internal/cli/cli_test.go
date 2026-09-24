package cli_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/cli"
)

func TestVersion(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	require.NoError(t, cli.RunContext(t.Context(), []string{"--version"}, &output))
	assert.Equal(t, "pg-tunnel dev\n", output.String())
}

func TestHelp(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"--help"}, {"help"}} {
		var output bytes.Buffer
		require.NoError(t, cli.RunContext(t.Context(), args, &output))
		for _, want := range []string{"pg-tunnel run [--config PATH] CONNECTION -- COMMAND", "pg-tunnel connect", "pg-tunnel init", "pg-tunnel cleanup", "-version"} {
			assert.Contains(t, output.String(), want)
		}
	}
}

func TestUsageMistakesExplainTheFix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		want string
		args []string
	}{
		{"missing command", nil},
		{`unknown command "bogus"`, []string{"bogus"}},
		{"connect needs a CONNECTION name", []string{"connect"}},
		{"put -- between the connection name and the command: pg-tunnel run dev -- psql", []string{"run", "dev", "psql"}},
		{"run needs a command", []string{"run", "dev"}},
		{"missing command after --", []string{"run", "dev", "--"}},
		{"put --config before the connection name", []string{"connect", "dev", "--config", "other.json"}},
		{"connect takes only a CONNECTION name", []string{"connect", "dev", "extra"}},
		{"cleanup takes no arguments", []string{"cleanup", "extra"}},
		{"init takes no arguments", []string{"init", "dev"}},
		{`unexpected argument "extra"`, []string{"--version", "extra"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := cli.RunContext(t.Context(), tc.args, &output)
			require.ErrorIs(t, err, cli.ErrUsage)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, output.String())
		})
	}
}

func TestRunChecksCommandBeforeConnecting(t *testing.T) {
	t.Parallel()
	err := cli.RunContext(t.Context(), []string{"run", "--config", "/nonexistent/pg-tunnel.json", "dev", "--", "pg-tunnel-missing-command"}, &bytes.Buffer{})
	require.ErrorContains(t, err, "find command before connecting")
	require.NotErrorIs(t, err, cli.ErrUsage)
}

func TestUnknownOption(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	err := cli.RunContext(t.Context(), []string{"--unknown"}, &output)
	require.ErrorIs(t, err, cli.ErrUsage)
	assert.Contains(t, output.String(), "flag provided but not defined")
}

func TestIncompleteCommands(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"connect"}, {"run", "bioml"}, {"--version", "unexpected"}} {
		t.Run("args="+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			require.ErrorIs(t, cli.RunContext(t.Context(), args, &output), cli.ErrUsage)
			assert.Empty(t, output.String())
		})
	}
}

func TestOutputFailure(t *testing.T) {
	t.Parallel()

	failure := errors.New("output unavailable")
	err := cli.RunContext(t.Context(), []string{"--version"}, failingWriter{err: failure})
	require.ErrorIs(t, err, failure)
}

type failingWriter struct {
	err error
}

func (writer failingWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func TestEmptyExplicitConfigIsRejected(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"run", "connect"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := cli.RunContext(t.Context(), []string{mode, "--config=", "dev"}, &output)
			require.ErrorContains(t, err, "--config requires a non-empty path")
		})
	}
}

func TestInitHelpRequiresNeitherTerminalNorAWS(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	require.NoError(t, cli.RunContext(t.Context(), []string{"init", "--help"}, &output))
	assert.Contains(t, output.String(), "-region")
	assert.Contains(t, output.String(), "-aws-profile")
	assert.Contains(t, output.String(), "\n  -profile string\n")
	assert.Contains(t, output.String(), "-config")
	for _, option := range []string{"--profile", "--aws-profile"} {
		require.ErrorIs(t, cli.RunContext(t.Context(), []string{"init", option, "dev", "unexpected"}, &output), cli.ErrUsage)
	}
}
