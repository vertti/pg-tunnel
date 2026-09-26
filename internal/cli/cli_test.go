package cli_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/cli"
)

func TestVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	require.NoError(t, cli.RunContext(t.Context(), []string{"--version"}, &stdout, &stderr))
	assert.Equal(t, "pg-tunnel dev\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestHelp(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"--help"}, {"help"}, {"run", "--help"}, {"connect", "-h"}, {"check", "--help"}} {
		var stdout, stderr bytes.Buffer
		require.NoError(t, cli.RunContext(t.Context(), args, &stdout, &stderr))
		assert.Contains(t, stdout.String(), "Usage:")
		assert.Empty(t, stderr.String())
	}
	var stdout bytes.Buffer
	require.NoError(t, cli.RunContext(t.Context(), []string{"--help"}, &stdout, io.Discard))
	for _, want := range []string{"pg-tunnel run [--config PATH] CONNECTION -- COMMAND", "pg-tunnel connect", "pg-tunnel check", "pg-tunnel init", "pg-tunnel cleanup", "-version"} {
		assert.Contains(t, stdout.String(), want)
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
		{"check needs a CONNECTION name", []string{"check"}},
		{"check takes only a CONNECTION name", []string{"check", "dev", "--", "psql"}},
		{"put --config before the connection name", []string{"check", "dev", "--config", "other.json"}},
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
			err := cli.RunContext(t.Context(), tc.args, &output, &output)
			require.ErrorIs(t, err, cli.ErrUsage)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, output.String())
		})
	}
}

func TestRunChecksCommandBeforeConnecting(t *testing.T) {
	t.Parallel()
	err := cli.RunContext(t.Context(), []string{"run", "--config", "/nonexistent/pg-tunnel.json", "dev", "--", "pg-tunnel-missing-command"}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "find command before connecting")
	require.NotErrorIs(t, err, cli.ErrUsage)
}

func TestUnknownOption(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"--unknown"}, {"run", "--unknown"}, {"init", "--unknown"}} {
		var stdout, stderr bytes.Buffer
		err := cli.RunContext(t.Context(), args, &stdout, &stderr)
		require.ErrorIs(t, err, cli.ErrUsage)
		assert.Contains(t, stderr.String(), "flag provided but not defined")
		assert.Contains(t, stderr.String(), "Usage:")
		assert.Empty(t, stdout.String())
	}
}

func TestIncompleteCommands(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"connect"}, {"run", "bioml"}, {"--version", "unexpected"}} {
		t.Run("args="+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			require.ErrorIs(t, cli.RunContext(t.Context(), args, &output, &output), cli.ErrUsage)
			assert.Empty(t, output.String())
		})
	}
}

func TestOutputFailure(t *testing.T) {
	t.Parallel()

	failure := errors.New("output unavailable")
	err := cli.RunContext(t.Context(), []string{"--version"}, failingWriter{err: failure}, io.Discard)
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
	for _, mode := range []string{"run", "connect", "check"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			err := cli.RunContext(t.Context(), []string{mode, "--config=", "dev"}, io.Discard, io.Discard)
			require.ErrorContains(t, err, "--config requires a non-empty path")
		})
	}
}

func TestInitHelpRequiresNeitherTerminalNorAWS(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	require.NoError(t, cli.RunContext(t.Context(), []string{"init", "--help"}, &output, io.Discard))
	assert.Contains(t, output.String(), "-region")
	assert.Contains(t, output.String(), "-aws-profile")
	assert.Contains(t, output.String(), "\n  -profile string\n")
	assert.Contains(t, output.String(), "-config")
	for _, option := range []string{"--profile", "--aws-profile"} {
		require.ErrorIs(t, cli.RunContext(t.Context(), []string{"init", option, "dev", "unexpected"}, io.Discard, io.Discard), cli.ErrUsage)
	}
}
