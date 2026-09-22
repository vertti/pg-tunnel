package libpq_test

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
)

func TestVerifyRequiresTrustedServerIdentity(t *testing.T) {
	t.Parallel()
	for _, validHost := range []bool{true, false} {
		t.Run(strconv.FormatBool(validHost), func(t *testing.T) {
			t.Parallel()
			certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
			certificateServer.Close()
			cert := certificateServer.Certificate()
			certPath := filepath.Join(t.TempDir(), "ca.pem")
			require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
			var config net.ListenConfig
			listener, err := config.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, listener.Close()) })
			address, ok := listener.Addr().(*net.TCPAddr)
			require.True(t, ok)
			serverResult := make(chan error, 1)
			go func() { serverResult <- servePostgres(t.Context(), listener, certificateServer.TLS) }()
			host := cert.DNSNames[0]
			if !validHost {
				host = "wrong.example"
			}
			err = libpq.Verify(t.Context(), session.Target{Host: host, Database: "data", User: "reader", RootCert: certPath}, address.Port, session.Credential{Secret: "test-token"})
			if validHost {
				require.NoError(t, err)
				require.NoError(t, <-serverResult)
			} else {
				require.ErrorContains(t, err, "TLS/database connection failed")
				assert.NotContains(t, err.Error(), "test-token")
				require.Error(t, <-serverResult)
			}
		})
	}
}

func servePostgres(ctx context.Context, listener net.Listener, config *tls.Config) error {
	conn, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close() //nolint:errcheck // The fixture has already completed or reported its protocol failure.
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("deadline: %w", err)
	}
	var request [8]byte
	if _, err = io.ReadFull(conn, request[:]); err != nil {
		return fmt.Errorf("SSL request: %w", err)
	}
	if _, err = conn.Write([]byte{'S'}); err != nil {
		return fmt.Errorf("SSL response: %w", err)
	}
	secure := tls.Server(conn, config)
	if err = secure.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	backend := pgproto3.NewBackend(secure, secure)
	if _, err = backend.ReceiveStartupMessage(); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	return authenticateClient(backend)
}

func authenticateClient(backend *pgproto3.Backend) error {
	backend.Send(&pgproto3.AuthenticationCleartextPassword{})
	if err := backend.Flush(); err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	message, err := backend.Receive()
	if err != nil {
		return fmt.Errorf("password: %w", err)
	}
	password, ok := message.(*pgproto3.PasswordMessage)
	if !ok || password.Password != "test-token" {
		return errors.New("wrong password")
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err = backend.Flush(); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	message, err = backend.Receive()
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	if _, ok = message.(*pgproto3.Terminate); !ok {
		return errors.New("expected terminate")
	}
	return nil
}
