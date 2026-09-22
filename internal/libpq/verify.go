package libpq

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/vertti/pg-tunnel/internal/session"
)

// Verify performs the RDS IAM authentication handshake over verified TLS.
// It reads no ambient libpq settings and sends no SQL queries.
func Verify(ctx context.Context, target session.Target, port int, credential session.Credential) error {
	config, err := TLSConfig(target)
	if err != nil {
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(verifyCtx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("dial local tunnel: %w", err)
	}
	defer conn.Close()                                                //nolint:errcheck // The verification connection has finished; close cannot invalidate the authentication result.
	stop := context.AfterFunc(verifyCtx, func() { _ = conn.Close() }) //nolint:errcheck // Closing the socket is how cancellation interrupts a blocked protocol read.
	defer stop()
	if err = authenticate(verifyCtx, conn, config, target, credential); err != nil {
		return fmt.Errorf("TLS/database connection failed (check CA, rds-db:connect, and database user rds_iam membership): %s", strings.ReplaceAll(err.Error(), credential.Secret, "[redacted]"))
	}
	return nil
}

// TLSConfig loads the configured trust roots before any AWS session is opened.
func TLSConfig(target session.Target) (*tls.Config, error) {
	cert, err := os.ReadFile(target.RootCert)
	if err != nil {
		return nil, fmt.Errorf("read sslrootcert %s: %w", target.RootCert, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		return nil, errors.New("sslrootcert contains no usable PEM certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: target.Host}, nil
}

func authenticate(ctx context.Context, raw net.Conn, config *tls.Config, target session.Target, credential session.Credential) error {
	request := pgproto3.NewFrontend(raw, raw)
	request.Send(&pgproto3.SSLRequest{})
	if err := request.Flush(); err != nil {
		return fmt.Errorf("request TLS: %w", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(raw, reply[:]); err != nil {
		return fmt.Errorf("read TLS response: %w", err)
	}
	if reply[0] != 'S' {
		return errors.New("database refused TLS; plaintext fallback is disabled")
	}
	secure := tls.Client(raw, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("verify TLS certificate: %w", err)
	}
	frontend := pgproto3.NewFrontend(secure, secure)
	frontend.SetMaxBodyLen(1 << 20)
	frontend.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber, Parameters: map[string]string{
		"user": target.User, "database": target.Database, "application_name": "pg-tunnel-verification",
	}})
	if err := frontend.Flush(); err != nil {
		return fmt.Errorf("send database startup: %w", err)
	}
	return completeAuthentication(frontend, credential.Secret)
}

func completeAuthentication(frontend *pgproto3.Frontend, secret string) error {
	challenged, authenticated := false, false
	for {
		message, err := frontend.Receive()
		if err != nil {
			return fmt.Errorf("read authentication response: %w", err)
		}
		switch response := message.(type) {
		case *pgproto3.AuthenticationCleartextPassword:
			challenged = true
			frontend.Send(&pgproto3.PasswordMessage{Password: secret})
			if err = frontend.Flush(); err != nil {
				return fmt.Errorf("send IAM token over TLS: %w", err)
			}
		case *pgproto3.AuthenticationOk:
			authenticated = challenged
		case *pgproto3.ReadyForQuery:
			return finishVerification(frontend, authenticated)
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("database rejected login (SQLSTATE %s): %s", response.Code, response.Message)
		case *pgproto3.ParameterStatus, *pgproto3.BackendKeyData, *pgproto3.NoticeResponse:
		default:
			return errors.New("database requested an unsupported authentication exchange; this backend requires RDS IAM authentication")
		}
	}
}

func finishVerification(frontend *pgproto3.Frontend, authenticated bool) error {
	if !authenticated {
		return errors.New("database did not authenticate the IAM token")
	}
	frontend.Send(&pgproto3.Terminate{})
	if err := frontend.Flush(); err != nil {
		return fmt.Errorf("end verification connection: %w", err)
	}
	return nil
}
