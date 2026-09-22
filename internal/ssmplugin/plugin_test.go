package ssmplugin_test

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/ssmplugin"
)

func TestRejectsUnsupportedChildInvocations(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"--version"},
		{"inline-secret", "region", "StartSession", "", "{}", "endpoint"},
		{ssmplugin.ResponseEnv, "region", "OtherOperation", "", "{}", "endpoint"},
	} {
		require.ErrorContains(t, ssmplugin.Run(args, io.Discard), "invalid internal SSM invocation")
	}
}
