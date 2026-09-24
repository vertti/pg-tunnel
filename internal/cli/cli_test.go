package cli_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/cli"
	"github.com/vertti/pg-tunnel/internal/profile"
)

func TestVersion(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	require.NoError(t, cli.Run([]string{"--version"}, &output))
	assert.Equal(t, "pg-tunnel dev\n", output.String())
}

func TestHelp(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	require.NoError(t, cli.Run([]string{"--help"}, &output))
	assert.Contains(t, output.String(), "Usage: pg-tunnel run")
	assert.Contains(t, output.String(), "-version")
}

func TestUnknownOption(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	err := cli.Run([]string{"--unknown"}, &output)
	require.ErrorContains(t, err, "parse options:")
	assert.Contains(t, output.String(), "flag provided but not defined")
}

func TestIncompleteCommands(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"connect"}, {"run", "bioml"}, {"--version", "unexpected"}} {
		t.Run("args="+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			require.ErrorIs(t, cli.Run(args, &output), cli.ErrUsage)
			assert.Empty(t, output.String())
		})
	}
}

func TestOutputFailure(t *testing.T) {
	t.Parallel()

	failure := errors.New("output unavailable")
	err := cli.Run([]string{"--version"}, failingWriter{err: failure})
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
			err := cli.Run([]string{mode, "--config=", "dev"}, &output)
			require.ErrorContains(t, err, "--config requires a non-empty path")
		})
	}
}

func TestInitHelpRequiresNeitherTerminalNorAWS(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	require.NoError(t, cli.Run([]string{"init", "--help"}, &output))
	assert.Contains(t, output.String(), "-region")
	assert.Contains(t, output.String(), "-aws-profile")
	assert.Contains(t, output.String(), "\n  -profile string\n")
	assert.Contains(t, output.String(), "-config")
	for _, option := range []string{"--profile", "--aws-profile"} {
		require.ErrorIs(t, cli.Run([]string{"init", option, "dev", "unexpected"}, &output), cli.ErrUsage)
	}
}

func TestProductionWarningBeforeConnection(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))
	for _, environment := range []string{"production", "staging", "development", ""} {
		path := filepath.Join(t.TempDir(), "config.json")
		p := profile.Profile{Environment: environment, DBInstance: "example", Database: "data", User: "reader", Target: "i-example", Port: 5432, AWSProfile: "missing-test-profile"}
		require.NoError(t, profile.Save(path, "test", &p))
		var output bytes.Buffer
		err := cli.Run([]string{"connect", "--config", path, "test"}, &output)
		require.ErrorContains(t, err, "load AWS configuration")
		assert.Equal(t, environment == "production", strings.Contains(output.String(), "WARNING: PRODUCTION"))
	}
}
