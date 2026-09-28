package awsdb

import (
	"strings"
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

func TestTokenIsRedactedBeforeTheTailIsTrimmed(t *testing.T) {
	t.Parallel()
	const token = "sensitive-token-value"
	for name, chunks := range map[string][]string{
		"trimmed inside token": {token + strings.Repeat("x", 8192-12)},
		"split across writes":  {"before " + token[:9], token[9:] + " after\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tail := logTail{token: token}
			for _, chunk := range chunks {
				_, err := tail.Write([]byte(chunk))
				require.NoError(t, err)
			}
			assert.NotContains(t, tail.String(), token[:10])
			assert.NotContains(t, tail.String(), token[len(token)-10:])
		})
	}
}
