package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRenewalDeadlinesUseWallClock(t *testing.T) {
	t.Parallel()
	expiry := time.Now().Add(15 * time.Minute)
	due := renewalTime(Credential{ExpiresAt: expiry})
	assert.Equal(t, due.Round(0), due, "a monotonic reading would ignore time spent asleep")
	assert.WithinDuration(t, expiry.Add(-renewalMargin), due, time.Second)

	soon := renewalTime(Credential{ExpiresAt: time.Now().Add(time.Minute)})
	assert.WithinDuration(t, time.Now().Add(minimumRenewal), soon, time.Second)
	password := renewalTime(Credential{})
	assert.WithinDuration(t, time.Now().Add(passwordPoll), password, time.Second)
}
