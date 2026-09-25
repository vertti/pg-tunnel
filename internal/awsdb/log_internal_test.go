package awsdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/ssmplugin"
)

func TestOnlySafeRecoveryMessagesAreForwarded(t *testing.T) {
	t.Parallel()
	var messages []string
	tail := logTail{report: func(s string) { messages = append(messages, s) }}
	for _, chunk := range []string{"private token\n", ssmplugin.RecoveryStarted[:12], ssmplugin.RecoveryStarted[12:] + "\n", "secret " + ssmplugin.RecoveryResumed + "\n", ssmplugin.RecoveryResumed + "\n"} {
		_, err := tail.Write([]byte(chunk))
		require.NoError(t, err)
	}
	assert.Equal(t, []string{ssmplugin.RecoveryStarted, ssmplugin.RecoveryResumed}, messages)
}
