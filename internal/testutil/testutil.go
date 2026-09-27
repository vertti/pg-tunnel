// Package testutil provides fixtures shared by tests in several packages.
package testutil

import (
	"encoding/pem"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// CA returns a PEM file with a certificate trusted by nothing else.
func CA(t *testing.T) string {
	t.Helper()
	path, _ := TLSServerCA(t)
	return path
}

// TLSServerCA writes the certificate of a closed TLS test server to a PEM file.
// The server's TLS configuration still serves that certificate.
func TLSServerCA(t *testing.T) (path string, server *httptest.Server) {
	t.Helper()
	server = httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	path = filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	return path, server
}

// IsolateHome points the user configuration and cache directories at a new
// temporary home.
func IsolateHome(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return home
}

// SeedRDSCA installs ca as a freshly downloaded commercial-region RDS bundle.
func SeedRDSCA(t *testing.T, ca string) {
	t.Helper()
	cache, err := os.UserCacheDir()
	require.NoError(t, err)
	managed := filepath.Join(cache, "pg-tunnel", "certificates", "aws.pem")
	require.NoError(t, os.MkdirAll(filepath.Dir(managed), 0o700))
	data, err := os.ReadFile(ca)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(managed, data, 0o600))
}

// SelfCommand runs one test of the current test binary in a subprocess with
// env added, contributing to the parent's coverage profile.
func SelfCommand(t *testing.T, test string, env ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), executable, "-test.run=^"+test+"$", coverageFlag())
	command.Env = append(os.Environ(), env...)
	return command
}

func coverageFlag() string {
	if dir := flag.Lookup("test.gocoverdir"); dir != nil && dir.Value.String() != "" {
		return "-test.gocoverdir=" + dir.Value.String()
	}
	return "-test.count=1"
}
