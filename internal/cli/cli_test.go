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
