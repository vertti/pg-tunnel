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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/session"
)

func TestVerifyRequiresTrustedPasswordAuthentication(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode, want string
		wrongHost, iam   bool
	}{
		{name: "cleartext over TLS"},
		{name: "IAM token", iam: true},
		{name: "MD5", mode: "md5"},
		{name: "wrong identity", wrongHost: true, want: "certificate"},
		{name: "no TLS", mode: "plaintext", want: "TLS"},
		{name: "trust is not password verification", mode: "trust", want: "require_auth"},
		{name: "redacted server error", mode: "error", want: "[redacted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			go func() { serverResult <- servePostgres(t.Context(), listener, certificateServer.TLS, tc.mode) }()
			host := cert.DNSNames[0]
			if tc.wrongHost {
				host = "wrong.example"
			}
			credential := session.Credential{Secret: "test-token"}
			if tc.iam {
				credential.ExpiresAt = time.Now().Add(time.Minute)
			}
			err = libpq.Verify(t.Context(), session.Target{Host: host, Database: "data", User: "reader", RootCert: certPath}, address.Port, credential, nil)
			serverErr := <-serverResult
			if tc.want == "" {
				require.NoError(t, err)
				require.NoError(t, serverErr)
			} else {
				require.ErrorContains(t, err, tc.want)
				assert.NotContains(t, err.Error(), credential.Secret)
			}
		})
	}
}

func servePostgres(ctx context.Context, listener net.Listener, config *tls.Config, mode string) error {
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
	reply := byte('S')
	if mode == "plaintext" {
		reply = 'N'
	}
	if _, err = conn.Write([]byte{reply}); err != nil {
		return fmt.Errorf("SSL response: %w", err)
	}
	if mode == "plaintext" {
		return nil
	}
	secure := tls.Server(conn, config)
	if err = secure.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	backend := pgproto3.NewBackend(secure, secure)
	if _, err = backend.ReceiveStartupMessage(); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	return authenticateClient(backend, mode)
}

func authenticateClient(backend *pgproto3.Backend, mode string) error {
	if mode == "error" {
		backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: "rejected test-token"})
		if err := backend.Flush(); err != nil {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	}
	if mode != "trust" {
		if err := challengeClient(backend, mode); err != nil {
			return err
		}
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	message, err := backend.Receive()
	if err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	if _, ok := message.(*pgproto3.Terminate); !ok {
		return errors.New("expected terminate")
	}
	return nil
}

func challengeClient(backend *pgproto3.Backend, mode string) error {
	expected := "test-token"
	if mode == "md5" {
		backend.Send(&pgproto3.AuthenticationMD5Password{Salt: [4]byte{1, 2, 3, 4}})
		expected = "md5dbcc9fd86b72cdc6c8dd49ccae226d80"
	} else {
		backend.Send(&pgproto3.AuthenticationCleartextPassword{})
	}
	if err := backend.Flush(); err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	message, err := backend.Receive()
	if err != nil {
		return fmt.Errorf("password: %w", err)
	}
	password, ok := message.(*pgproto3.PasswordMessage)
	if !ok || password.Password != expected {
		return errors.New("wrong password")
	}
	return nil
}
