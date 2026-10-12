package setup

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/testutil"
)

func TestCommandOmitsDefaultConfig(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	testutil.IsolateHome(t)
	user, err := profile.UserPath()
	require.NoError(t, err)
	assert.Equal(t, "pg-tunnel run 'dev'", command("run", user, "dev"))
	assert.Equal(t, "pg-tunnel run --config 'other.json' 'dev'", command("run", "other.json", "dev"))
	require.NoError(t, os.WriteFile("pg-tunnel.json", []byte("{}"), 0o600))
	assert.Equal(t, "pg-tunnel run --config '"+user+"' 'dev'", command("run", user, "dev"), "a project file would shadow the user configuration")
}

func TestFreeLocalPortSkipsSavedAndBusyPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	port, err := freeLocalPort(t.Context(), path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, port, firstLocalPort)
	saved := profile.Profile{DBInstance: "db", Database: "data", User: "reader", Target: "i-1", Port: 5432, LocalPort: port}
	require.NoError(t, profile.Save(path, "first", &saved))
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
	require.NoError(t, err)
	defer busy.Close() //nolint:errcheck // Test listener.
	next, err := freeLocalPort(t.Context(), path)
	require.NoError(t, err)
	assert.Greater(t, next, port+1)
}
