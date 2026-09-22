package profile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/profile"
)

const valid = `{"profiles":{"dev":{"db_instance":"example","database":"data","user":"reader","target":"i-example","sslrootcert":"ca.pem"}}}`

func TestLoadResolvesDefaultsAndCAPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pg-tunnel.json")
	require.NoError(t, os.WriteFile(path, []byte(valid), 0o600))
	value, err := profile.Load(path, "dev")
	require.NoError(t, err)
	assert.Equal(t, 5432, value.Port)
	assert.Zero(t, value.LocalPort)
	assert.Equal(t, filepath.Join(dir, "ca.pem"), value.RootCert)
}

func TestInvalidProfilesFailBeforeAWSAccess(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{
		"unknown field":  strings.Replace(valid, `"user"`, `"username"`, 1),
		"ambiguous host": strings.Replace(valid, `"database"`, `"host":"db.example","database"`, 1),
		"missing jump":   strings.Replace(valid, `"target":"i-example",`, "", 1),
		"bad port":       strings.Replace(valid, `"database"`, `"local_port":70000,"database"`, 1),
		"injection":      strings.Replace(valid, `"reader"`, `"reader\nsslmode=disable"`, 1),
		"wildcard":       strings.Replace(valid, `"reader"`, `"*"`, 1),
		"extra object":   valid + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "profiles.json")
			require.NoError(t, os.WriteFile(path, []byte(input), 0o600))
			_, err := profile.Load(path, "dev")
			require.Error(t, err)
		})
	}
}
