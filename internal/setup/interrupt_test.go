package setup_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/setup"
)

// Secrets Manager with every suggested default: database, jump host, auth, secret,
// database name, user, environment, connection name, confirmation.
var passwordAnswers = []string{"1", "1", "2", "", "", "", "", "readonly", "yes"}

func TestWizardWritesNothingWhenInputEndsAtAnyPrompt(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	for answered := range passwordAnswers {
		path := filepath.Join(t.TempDir(), "profiles.json")
		input := strings.Join(passwordAnswers[:answered], "\n")
		if answered > 0 {
			input += "\n"
		}
		wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, ""), Input: strings.NewReader(input), Output: io.Discard, Path: path, RootCert: ca}
		require.ErrorContains(t, wizard.Run(t.Context()), "input ended", "after %d answers", answered)
		assert.NoFileExists(t, path)
	}
}

type limitedWriter struct{ remaining int }

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	w.remaining--
	return len(data), nil
}

func TestWizardReportsEveryOutputFailure(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	input := strings.Join(passwordAnswers, "\n") + "\n"
	for writes := 0; ; writes++ {
		require.Less(t, writes, 200, "the wizard never completed")
		path := filepath.Join(t.TempDir(), "profiles.json")
		wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, ""), Input: strings.NewReader(input), Output: &limitedWriter{remaining: writes}, Path: path, RootCert: ca}
		err := wizard.Run(t.Context())
		if err == nil {
			break
		}
		require.ErrorIs(t, err, io.ErrClosedPipe, "after %d writes", writes)
	}
}

func TestWizardRequiresUniqueName(t *testing.T) {
	t.Parallel()
	ca := certificate(t)
	input := strings.Join(passwordAnswers, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "profiles.json")
	existing := profile.Profile{DBInstance: "other", Database: "data", User: "reader", Target: "i-other", Port: 5432}
	require.NoError(t, profile.Save(path, "readonly", &existing))
	wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, ""), Input: strings.NewReader(input), Output: io.Discard, Path: path, RootCert: ca}
	require.ErrorContains(t, wizard.Run(t.Context()), "already exists")
	saved, err := profile.Load(path, "readonly")
	require.NoError(t, err)
	assert.Equal(t, "other", saved.DBInstance)
}

func TestWizardPreparesManagedCertificates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	cache, err := os.UserCacheDir()
	require.NoError(t, err)
	managed := filepath.Join(cache, "pg-tunnel", "certificates", "aws.pem")
	require.NoError(t, os.MkdirAll(filepath.Dir(managed), 0o700))
	data, err := os.ReadFile(certificate(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(managed, data, 0o600)) //nolint:gosec // The path is inside the test's temporary directory.

	path := filepath.Join(t.TempDir(), "profiles.json")
	input := strings.Join(passwordAnswers, "\n") + "\n"
	wizard := setup.Wizard{Verify: successfulVerification, Config: fixtureConfig(t, ""), Input: strings.NewReader(input), Output: io.Discard, Path: path}
	require.NoError(t, wizard.Run(t.Context()))
	saved, err := profile.Load(path, "readonly")
	require.NoError(t, err)
	assert.Empty(t, saved.RootCert, "managed certificates are not saved into profiles")
}
