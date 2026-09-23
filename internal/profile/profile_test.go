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

func TestLoadConfigurationPrecedence(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	t.Setenv("HOME", filepath.Join(directory, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	userDirectory, err := os.UserConfigDir()
	require.NoError(t, err)
	userPath := filepath.Join(userDirectory, "pg-tunnel", "pg-tunnel.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(userPath), 0o700))
	require.NoError(t, os.WriteFile(userPath, []byte(valid), 0o600))

	// A shared profile and its relative CA work from different worktrees.
	for _, worktree := range []string{"first", "second"} {
		t.Run(worktree, func(t *testing.T) {
			path := filepath.Join(directory, worktree)
			require.NoError(t, os.Mkdir(path, 0o700))
			t.Chdir(path)
			value, loadErr := profile.Load("", "dev")
			require.NoError(t, loadErr)
			assert.Equal(t, filepath.Join(filepath.Dir(userPath), "ca.pem"), value.RootCert)
		})
	}

	require.NoError(t, os.WriteFile("pg-tunnel.json", []byte(strings.Replace(valid, `"reader"`, `"project-reader"`, 1)), 0o600))
	value, err := profile.Load("", "dev")
	require.NoError(t, err)
	assert.Equal(t, "project-reader", value.User)
	assert.Equal(t, filepath.Join(directory, "ca.pem"), value.RootCert)

	explicitPath := filepath.Join(directory, "explicit.json")
	require.NoError(t, os.WriteFile(explicitPath, []byte(strings.Replace(valid, `"reader"`, `"explicit-reader"`, 1)), 0o600))
	value, err = profile.Load(explicitPath, "dev")
	require.NoError(t, err)
	assert.Equal(t, "explicit-reader", value.User)
	_, err = profile.Load(filepath.Join(directory, "missing.json"), "dev")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLoadDoesNotFallBackFromExistingProjectConfig(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	t.Setenv("HOME", filepath.Join(directory, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	userDirectory, err := os.UserConfigDir()
	require.NoError(t, err)
	userPath := filepath.Join(userDirectory, "pg-tunnel", "pg-tunnel.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(userPath), 0o700))
	require.NoError(t, os.WriteFile(userPath, []byte(valid), 0o600))

	for name, content := range map[string]string{
		"malformed":       "{",
		"missing profile": `{"profiles":{}}`,
		"invalid profile": strings.Replace(valid, `"reader"`, `""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile("pg-tunnel.json", []byte(content), 0o600))
			_, loadErr := profile.Load("", "dev")
			require.ErrorContains(t, loadErr, "pg-tunnel.json")
		})
	}
	require.NoError(t, os.Remove("pg-tunnel.json"))
	require.NoError(t, os.Symlink("missing.json", "pg-tunnel.json"))
	_, err = profile.Load("", "dev")
	require.ErrorIs(t, err, os.ErrNotExist, "a broken project symlink must not select the shared profile")
}

func TestLoadMissingConfigurationReportsLocations(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	userDirectory, err := os.UserConfigDir()
	require.NoError(t, err)
	_, err = profile.Load("", "dev")
	require.ErrorContains(t, err, "pg-tunnel.json in the current directory")
	require.ErrorContains(t, err, filepath.Join(userDirectory, "pg-tunnel", "pg-tunnel.json"))
	require.ErrorContains(t, err, "--config PATH")
}

func TestProjectAndExplicitConfigDoNotRequireHome(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	_, err := profile.Load("", "dev")
	require.ErrorContains(t, err, "find user configuration directory")
	require.NoError(t, os.WriteFile("pg-tunnel.json", []byte(valid), 0o600))
	_, err = profile.Load("", "dev")
	require.NoError(t, err)
	_, err = profile.Load("pg-tunnel.json", "dev")
	require.NoError(t, err)
}

func TestAutomaticCAIsOnlyAvailableForRDSInstances(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profiles.json")
	content := strings.Replace(valid, `,"sslrootcert":"ca.pem"`, "", 1)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	value, err := profile.Load(path, "dev")
	require.NoError(t, err)
	assert.Empty(t, value.RootCert, "an automatic CA must not resolve to the configuration directory")
	content = strings.Replace(content, `"db_instance":"example"`, `"host":"db.example"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	_, err = profile.Load(path, "dev")
	require.ErrorContains(t, err, "sslrootcert is required for an explicit host")
}

func TestExplicitAuthenticationConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ auth, secret, want string }{
		{"", "", ""},
		{"iam", "", ""},
		{"secrets-manager", "chosen", ""},
		{"", "chosen", "secret_id requires"},
		{"iam", "chosen", "secret_id requires"},
		{"secrets-manager", "", "requires secret_id"},
		{"unknown", "", "auth must"},
	} {
		t.Run(tc.auth+"/"+tc.secret, func(t *testing.T) {
			t.Parallel()
			p := profile.Profile{DBInstance: "example", Database: "data", User: "reader", Target: "i-example", Port: 5432, Auth: tc.auth, SecretID: tc.secret}
			err := p.Validate()
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
