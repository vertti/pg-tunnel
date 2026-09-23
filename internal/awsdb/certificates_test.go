package awsdb //nolint:testpackage // Exercise the cache against a private HTTPS test server without making the production trust source configurable.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCA(t *testing.T, expires time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: expires, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestRDSCACacheDownloadsReusesRefreshesAndPreservesLastGoodBundle(t *testing.T) {
	t.Parallel()
	original := testCA(t, time.Now().Add(time.Hour))
	replacement := testCA(t, time.Now().Add(2*time.Hour))
	var requests atomic.Int32
	var fail atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := requests.Add(1)
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		data := original
		if count > 1 {
			data = replacement
		}
		_, err := w.Write(data)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "certificates", "aws.pem")
	var messages []string
	report := func(message string) { messages = append(messages, message) }
	result, err := cachedCA(t.Context(), server.Client(), path, server.URL, report)
	require.NoError(t, err)
	assert.Equal(t, path, result)
	result, err = cachedCA(t.Context(), server.Client(), path, server.URL, report)
	require.NoError(t, err)
	assert.Equal(t, path, result)
	assert.EqualValues(t, 1, requests.Load(), "fresh cache needs no network request")
	stale := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(path, stale, stale))
	_, err = cachedCA(t.Context(), server.Client(), path, server.URL, report)
	require.NoError(t, err)
	actual, err := os.ReadFile(path) //nolint:gosec // The cache is inside t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, replacement, actual)
	require.NoError(t, os.Chtimes(path, stale, stale))
	fail.Store(true)
	_, err = cachedCA(t.Context(), server.Client(), path, server.URL, report)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(messages, "\n"), "using the cached bundle")
	actual, err = os.ReadFile(path) //nolint:gosec // The cache is inside t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, replacement, actual)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = cachedCA(ctx, server.Client(), path, server.URL, report)
	require.ErrorIs(t, err, context.Canceled, "cancellation must not continue with the stale cache")
}

func TestRDSCARejectsBadDownloads(t *testing.T) {
	t.Parallel()
	expired := testCA(t, time.Now().Add(-time.Hour))
	for name, data := range map[string][]byte{
		"empty": {}, "html": []byte("<html>error</html>"), "expired": expired,
		"too large":       bytes.Repeat([]byte("x"), maxBundleSize+1),
		"bad certificate": []byte("-----BEGIN CERTIFICATE-----\naGVsbG8=\n-----END CERTIFICATE-----\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, err := w.Write(data); assert.NoError(t, err) }))
			t.Cleanup(server.Close)
			path := filepath.Join(t.TempDir(), "ca.pem")
			_, err := cachedCA(t.Context(), server.Client(), path, server.URL, nil)
			require.ErrorContains(t, err, "configure sslrootcert")
			assert.NoFileExists(t, path)
			require.NoError(t, os.WriteFile(path, expired, 0o600))
			_, err = cachedCA(t.Context(), server.Client(), path, server.URL, nil)
			require.Error(t, err, "an expired cache must not mask a failed download")
		})
	}
}

func TestRDSCADownloadRequiresHTTPSAndRejectsRedirects(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/ca.pem", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	_, err := downloadCA(t.Context(), server.Client(), "http://example.invalid/ca.pem")
	require.ErrorContains(t, err, "require HTTPS")
	_, err = downloadCA(t.Context(), server.Client(), server.URL)
	require.ErrorContains(t, err, "HTTP 302")
	_, err = downloadCA(t.Context(), &http.Client{}, server.URL)
	require.ErrorContains(t, err, "certificate", "the downloader must verify its HTTPS peer")
}

func TestRDSCACorruptCacheIsReplaced(t *testing.T) {
	t.Parallel()
	data := testCA(t, time.Now().Add(time.Hour))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, err := w.Write(data); assert.NoError(t, err) }))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte("broken"), 0o600))
	_, err := cachedCA(t.Context(), server.Client(), path, server.URL, nil)
	require.NoError(t, err)
	actual, err := os.ReadFile(path) //nolint:gosec // The cache is inside t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, data, actual)
}

func TestRDSCASources(t *testing.T) {
	t.Parallel()
	for _, region := range []string{"eu-central-1", "us-gov-east-1", "cn-north-1"} {
		t.Run(region, func(t *testing.T) {
			t.Parallel()
			name, source := certificateSource(region)
			assert.True(t, strings.HasPrefix(source, "https://"), region+" must use HTTPS")
			assert.NotContains(t, name, "/")
		})
	}
}
