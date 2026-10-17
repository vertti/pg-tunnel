package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRenewalDeadlinesUseWallClock(t *testing.T) {
	t.Parallel()
	expiry := time.Now().Add(15 * time.Minute)
	var runner Runner
	due := runner.renewalTime(Credential{ExpiresAt: expiry})
	assert.Equal(t, due.Round(0), due, "a monotonic reading would ignore time spent asleep")
	assert.WithinDuration(t, expiry.Add(-renewalMargin), due, time.Second)

	soon := runner.renewalTime(Credential{ExpiresAt: time.Now().Add(time.Minute)})
	assert.WithinDuration(t, time.Now().Add(minimumRenewal), soon, time.Second)
	password := runner.renewalTime(Credential{})
	assert.WithinDuration(t, time.Now().Add(defaultPasswordPoll), password, time.Second)
	runner.PasswordPoll = time.Second
	assert.WithinDuration(t, time.Now().Add(time.Second), runner.renewalTime(Credential{}), 100*time.Millisecond)
}
