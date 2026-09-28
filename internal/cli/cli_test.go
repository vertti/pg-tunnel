package cli_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

	for _, args := range [][]string{{"--help"}, {"help"}, {"run", "--help"}, {"connect", "-h"}, {"check", "--help"}, {"cleanup", "--help"}, {"help", "check"}, {"help", "init"}} {
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
	stdout.Reset()
	require.NoError(t, cli.RunContext(t.Context(), []string{"help", "run"}, &stdout, io.Discard))
	assert.True(t, strings.HasPrefix(stdout.String(), "Usage: pg-tunnel run "), stdout.String())
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
		{"put -config before the connection name", []string{"run", "dev", "-config", "other.json", "--", "psql"}},
		{"put --help before the connection name", []string{"connect", "dev", "--help"}},
		{`unknown command "bogus"`, []string{"help", "bogus"}},
		{"flag provided but not defined: -profile", []string{"init", "--profile", "dev"}},
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
			assert.NotContains(t, err.Error(), "usage:")
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

	for args, want := range map[string]string{
		"--unknown":         "flag provided but not defined: -unknown (see pg-tunnel --help)",
		"run --unknown":     "flag provided but not defined: -unknown (see pg-tunnel run --help)",
		"init --unknown":    "flag provided but not defined: -unknown (see pg-tunnel init --help)",
		"cleanup --unknown": "flag provided but not defined: -unknown (see pg-tunnel cleanup --help)",
	} {
		var output bytes.Buffer
		err := cli.RunContext(t.Context(), strings.Fields(args), &output, &output)
		require.ErrorIs(t, err, cli.ErrUsage)
		assert.Equal(t, want, err.Error(), "one line, no usage prefix")
		assert.Empty(t, output.String(), "the error is printed once, by the caller")
	}
}

func TestUsageErrorsPointAtTheCommandHelp(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{
		"bogus":         `unknown command "bogus" (see pg-tunnel --help)`,
		"connect":       "connect needs a CONNECTION name: pg-tunnel connect [--config PATH] CONNECTION (see pg-tunnel connect --help)",
		"run dev":       "run needs a command: pg-tunnel run dev -- psql (see pg-tunnel run --help)",
		"init dev":      "init takes no arguments; pass the AWS profile with --aws-profile (see pg-tunnel init --help)",
		"cleanup extra": "cleanup takes no arguments (see pg-tunnel cleanup --help)",
	} {
		err := cli.RunContext(t.Context(), strings.Fields(args), io.Discard, io.Discard)
		require.ErrorIs(t, err, cli.ErrUsage)
		assert.Equal(t, want, err.Error())
	}
}

func TestConnectionErrorsAreNotRewrapped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pg-tunnel.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"profiles":{"dev":{"db_instance":"example","database":"data","user":"","target":"i-example"}}}`), 0o600))
	err := cli.RunContext(t.Context(), []string{"check", "--config", path, "dev"}, io.Discard, io.Discard)
	assert.EqualError(t, err, `connection "dev" in `+path+": user is required")
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
	for _, mode := range []string{"run", "connect", "check", "init"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			err := cli.RunContext(t.Context(), []string{mode, "--config="}, io.Discard, io.Discard)
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
	assert.NotContains(t, output.String(), "\n  -profile string\n")
	assert.Contains(t, output.String(), "-config")
}
