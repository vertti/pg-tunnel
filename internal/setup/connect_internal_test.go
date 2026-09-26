package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/profile"
)

func TestConnectCommandOmitsDefaultConfig(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	t.Setenv("HOME", filepath.Join(directory, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(directory, "config"))
	user, err := profile.UserPath()
	require.NoError(t, err)
	assert.Equal(t, "pg-tunnel run 'dev' -- psql", connectCommand(user, "dev"))
	assert.Equal(t, "pg-tunnel run --config 'other.json' 'dev' -- psql", connectCommand("other.json", "dev"))
	require.NoError(t, os.WriteFile("pg-tunnel.json", []byte("{}"), 0o600))
	assert.Equal(t, "pg-tunnel run --config '"+user+"' 'dev' -- psql", connectCommand(user, "dev"), "a project file would shadow the user configuration")
}
